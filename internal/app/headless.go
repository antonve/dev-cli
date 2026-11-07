package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/antonve/dev-cli/internal/bazel"
	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/execx"
	"github.com/antonve/dev-cli/internal/kube"
)

// headlessFlags select teardown from a route's lifecycle marker alone, for a
// Kubernetes Job that has cluster credentials but no checkout, config or Bazel.
type headlessFlags struct {
	enabled                       bool
	namespace, route, kubeContext string
}

func (h *headlessFlags) register(f *flag.FlagSet, route bool) {
	f.BoolVar(&h.enabled, "headless", false, "tear down from the lifecycle marker alone; reads no checkout, config or Bazel")
	f.StringVar(&h.namespace, "namespace", "", "routing namespace holding the lifecycle marker (with --headless)")
	f.StringVar(&h.kubeContext, "context", "", "kube context (with --headless; default: in-cluster or current context)")
	if route {
		f.StringVar(&h.route, "route", "", "route to tear down (with --headless)")
	}
}

// set reports whether any headless flag was given.
func (h *headlessFlags) set() bool {
	return h.enabled || h.namespace != "" || h.route != "" || h.kubeContext != ""
}

func (h *headlessFlags) run(ctx context.Context, command string, c *common, r execx.Runner, stdout io.Writer) error {
	if !h.enabled {
		return errors.New("--namespace, --route and --context require --headless")
	}
	if c.config != "" || c.owner != "" {
		return errors.New("--headless tears down under the marker's recorded owner and reads no config; drop --config and --owner")
	}
	if h.namespace == "" || command == "down" && h.route == "" {
		usage := "dev cleanup --headless --namespace <routing-namespace> [--context <kube-context>]"
		if command == "down" {
			usage = "dev down --headless --namespace <routing-namespace> --route <route> [--context <kube-context>]"
		}
		return errors.New("usage: " + usage)
	}
	k := kube.Client{Run: r, Context: h.kubeContext, Namespace: h.namespace}
	if command == "cleanup" {
		n, err := cleanupHeadless(ctx, k, time.Now(), stdout)
		fmt.Fprintf(stdout, "removed %d expired route(s)\n", n)
		return err
	}
	if err := teardownRoute(ctx, k, nil, "", h.route, stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "removed route %s\n", h.route)
	return nil
}

// headlessReady refuses markers that cannot be torn down without a checkout,
// such as those written by a CLI that did not record teardown data.
func headlessReady(route string, m *kube.Lifecycle) error {
	if m.Teardown == nil {
		return fmt.Errorf("route %s has a lifecycle marker from an older dev CLI without recorded teardown data; run dev down from its checkout", route)
	}
	for _, name := range m.Hooks.TaskNames() {
		if _, ok := m.Teardown.Tasks[name]; !ok {
			return fmt.Errorf("route %s marker has no recorded Job for task %q; run dev down from its checkout", route, name)
		}
	}
	return nil
}

// cleanupHeadless tears down expired routes that have a self-sufficient
// marker in the routing namespace. Other expired routes are left untouched.
func cleanupHeadless(ctx context.Context, k kube.Client, now time.Time, w io.Writer) (int, error) {
	seen := map[string]map[string]time.Time{}
	expiries := func(namespace string) (map[string]time.Time, error) {
		if seen[namespace] == nil {
			found, err := k.In(namespace).Cleanup(ctx, now)
			if err != nil {
				return nil, err
			}
			seen[namespace] = found
		}
		return seen[namespace], nil
	}
	routing, err := expiries(k.Namespace)
	if err != nil {
		return 0, err
	}
	routes := make([]string, 0, len(routing))
	for route, expiry := range routing {
		if now.After(expiry) {
			routes = append(routes, route)
		}
	}
	sort.Strings(routes)
	removed := 0
	for _, route := range routes {
		marker, err := k.ReadLifecycle(ctx, route)
		if err != nil {
			return removed, err
		}
		if marker == nil {
			fmt.Fprintf(w, "warning: route %s skipped: no lifecycle marker; run dev cleanup from a checkout\n", route)
			continue
		}
		if err := headlessReady(route, marker); err != nil {
			fmt.Fprintf(w, "warning: %v\n", err)
			continue
		}
		// A live overlay in a workload namespace keeps the route alive.
		expiry := routing[route]
		for _, namespace := range marker.Teardown.Namespaces {
			found, err := expiries(namespace)
			if err != nil {
				return removed, err
			}
			if found[route].After(expiry) {
				expiry = found[route]
			}
		}
		if !now.After(expiry) {
			continue
		}
		if err := teardownRoute(ctx, k, nil, "", route, w); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func runRecordedHooks(ctx context.Context, k kube.Client, recorded kube.Teardown, tasks []string, revision, owner, route string, w io.Writer) error {
	for _, name := range tasks {
		task := recorded.Tasks[name]
		for _, dependencyName := range task.Dependencies {
			found := false
			for _, dependency := range recorded.Dependencies {
				if dependency.Name == dependencyName {
					if err := k.ProvisionRecorded(ctx, dependency, owner, route); err != nil {
						return err
					}
					found = true
				}
			}
			if !found {
				return fmt.Errorf("task %s has unrecorded dependency %q", name, dependencyName)
			}
		}
		if err := k.RunRecordedTask(ctx, name, task, recorded.TaskLockNamespace, revision, owner, route); err != nil {
			return err
		}
		fmt.Fprintln(w, "completed task", name)
	}
	return nil
}

// recordTeardown publishes the images of the route's teardown tasks and
// renders everything teardown needs into the marker's Teardown record.
func recordTeardown(ctx context.Context, k kube.Client, bz bazel.Bazel, cfg config.Config, revision, route string, w io.Writer) (*kube.Teardown, error) {
	recorded := &kube.Teardown{Namespaces: cfg.Namespaces, TaskLockNamespace: cfg.TaskLockNamespace, Tasks: map[string]kube.RecordedTask{}}
	names := append([]string{}, cfg.Hooks.AfterDown...)
	for _, hooks := range cfg.Hooks.Deployables {
		names = append(names, hooks.AfterStop...)
	}
	needed := map[string]bool{}
	for _, name := range names {
		if _, done := recorded.Tasks[name]; done {
			continue
		}
		task, ok := cfg.Task(name)
		if !ok {
			return nil, fmt.Errorf("unknown task %q", name)
		}
		image := ""
		if task.PushTarget != "" {
			var err error
			if image, err = publish(ctx, bz, cfg, task.ImageName, task.PushTarget, route+"-"+shortRevision(revision), w); err != nil {
				return nil, err
			}
			fmt.Fprintf(w, "resolved teardown task %s to %s\n", name, image)
		}
		rendered, err := k.RenderTask(task, image, route)
		if err != nil {
			return nil, err
		}
		recorded.Tasks[name] = rendered
		for _, dependency := range task.Dependencies {
			needed[dependency] = true
		}
	}
	for _, d := range cfg.Dependencies {
		if d.Retention != "down" && !needed[d.Name] {
			continue
		}
		rendered, err := k.RenderDependency(d, route)
		if err != nil {
			return nil, err
		}
		recorded.Dependencies = append(recorded.Dependencies, rendered)
	}
	return recorded, nil
}
