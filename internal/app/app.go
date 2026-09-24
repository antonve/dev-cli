package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/antonve/dev-cli/internal/bazel"
	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/deeplink"
	"github.com/antonve/dev-cli/internal/execx"
	"github.com/antonve/dev-cli/internal/gitx"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/localstate"
	"github.com/antonve/dev-cli/internal/naming"
	"github.com/antonve/dev-cli/internal/registry"
	"github.com/antonve/dev-cli/internal/syncer"
)

const version = "v0.4.0"

type common struct{ config, owner, base string }
type stringsFlag []string

func (v *stringsFlag) String() string { return strings.Join(*v, ",") }
func (v *stringsFlag) Set(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	*v = append(*v, value)
	return nil
}

func flags(name string) (*flag.FlagSet, *common) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &common{}
	f.StringVar(&c.config, "config", "", "repository config (discover .dev/config.yaml or .dev/config.json)")
	f.StringVar(&c.owner, "owner", "", "owner identity")
	f.StringVar(&c.base, "base", "origin/main", "comparison base")
	return f, c
}
func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: dev <doctor|up|provision|task|url|status|logs|down|cleanup|version> [options]")
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("command required")
	}
	if args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return nil
	}
	r := execx.OS{}
	g := gitx.Git{Run: r}
	root, err := g.Root(ctx)
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	f, c := flags(args[0])
	noWatch := false
	clearSelection := false
	urlHost := ""
	var selectedServices, selectedDependencies, selectedTasks stringsFlag
	if args[0] == "url" {
		f.BoolVar(&clearSelection, "clear", false, "link to base and clear the branch cookie")
		f.StringVar(&urlHost, "host", "", "configured public host")
	}
	if args[0] == "up" {
		f.BoolVar(&noWatch, "no-watch", false, "create and initially sync overlays, then exit")
		f.Var(&selectedServices, "service", "explicitly include an unchanged service (repeatable)")
		f.Var(&selectedDependencies, "dependency", "provision a declared dependency (repeatable)")
		f.Var(&selectedTasks, "task", "run a declared task before startup (repeatable)")
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	service := ""
	if args[0] == "logs" && f.NArg() > 0 {
		service = f.Arg(0)
	}
	cfg, err := config.Load(c.config)
	if err != nil {
		return err
	}
	if c.owner == "" {
		c.owner = g.Owner(ctx)
	}
	branch, err := g.Branch(ctx)
	if err != nil {
		return err
	}
	route := naming.RouteKey(c.owner, branch)
	if args[0] == "url" {
		if f.NArg() > 1 {
			return errors.New("usage: dev url [--owner owner] [--clear] [/path?query#fragment]")
		}
		selection := route
		if clearSelection {
			selection = deeplink.Base
		}
		if urlHost == "" {
			urlHost = cfg.IngressHost
		}
		if !cfg.AllowsHost(urlHost) {
			return fmt.Errorf("host %q is not in publicHosts", urlHost)
		}
		link, err := deeplink.URL(urlHost, f.Arg(0), selection)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, link)
		return nil
	}
	k := kube.Client{Run: r, Context: cfg.KubeContext, Namespace: cfg.Namespace}
	bz := bazel.Bazel{Run: r, Args: cfg.BazelArgs}
	local := localstate.New(root, route)
	switch args[0] {
	case "doctor":
		return doctor(ctx, r, cfg, c.base, stdout)
	case "cleanup":
		n, err := cleanup(ctx, k, cfg, time.Now())
		fmt.Fprintf(stdout, "removed %d expired route(s)\n", n)
		return err
	case "down":
		stopped, err := local.Stop()
		if err != nil {
			return fmt.Errorf("stop local loop: %w", err)
		}
		for _, namespace := range cfg.Namespaces {
			if err := k.In(namespace).Down(ctx, c.owner, route); err != nil {
				return err
			}
		}
		for _, dependency := range cfg.Dependencies {
			if err := k.RemoveDependency(ctx, dependency, c.owner, route); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "removed route %s local-loop-stopped=%t\n", route, stopped)
		return nil
	case "logs":
		return logs(ctx, k, cfg, route, service, stdout, stderr)
	case "status":
		_, _ = cleanup(ctx, k, cfg, time.Now())
		return status(ctx, k, cfg, c.owner, branch, route, local.Running(), stdout)
	case "provision":
		if f.NArg() == 0 {
			return fmt.Errorf("usage: dev provision [flags] dependency...")
		}
		for _, name := range f.Args() {
			d, ok := cfg.Dependency(name)
			if !ok {
				return fmt.Errorf("unknown dependency %q", name)
			}
			if err := k.Provision(ctx, d, c.owner, route); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "ready dependency", name)
		}
		return nil
	case "task":
		if f.NArg() != 1 {
			return fmt.Errorf("usage: dev task [flags] name")
		}
		rev, err := g.Revision(ctx)
		if err != nil {
			return err
		}
		return runTask(ctx, k, bz, cfg, f.Arg(0), rev, c.owner, route, stdout)
	case "up":
		openURL, err := deeplink.URL(cfg.IngressHost, "/", route)
		if err != nil {
			return err
		}
		baseURL, _ := deeplink.URL(cfg.IngressHost, "/", deeplink.Base)
		upCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		release, err := local.Start(stop)
		if err != nil {
			return err
		}
		defer release()
		ctx = upCtx
		_, _ = cleanup(ctx, k, cfg, time.Now())
		merge, err := g.MergeBase(ctx, c.base)
		if err != nil {
			return err
		}
		changed, err := g.Changed(ctx, merge)
		if err != nil {
			return err
		}
		ds, err := bz.Metadata(ctx, cfg.MetadataQuery, changed)
		if err != nil {
			return err
		}
		allDeployables, err := bz.AllMetadata(ctx, cfg.MetadataQuery)
		if err != nil {
			return err
		}
		if err := validateDeployables(cfg, allDeployables); err != nil {
			return err
		}
		selected := map[string]config.Deployable{}
		for _, d := range ds {
			selected[d.Name] = d
		}
		for _, name := range selectedServices {
			d, ok := findDeployable(allDeployables, name)
			if !ok {
				return fmt.Errorf("unknown service %q", name)
			}
			selected[name] = d
		}
		ds = ds[:0]
		for _, d := range allDeployables {
			if _, ok := selected[d.Name]; ok {
				ds = append(ds, d)
			}
		}
		if len(ds) == 0 {
			fmt.Fprintln(stdout, "no affected deployables")
			return nil
		}
		rev, err := g.Revision(ctx)
		if err != nil {
			return err
		}
		for _, name := range selectedDependencies {
			d, ok := cfg.Dependency(name)
			if !ok {
				return fmt.Errorf("unknown dependency %q", name)
			}
			if err := k.Provision(ctx, d, c.owner, route); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "ready dependency", name)
		}
		for _, name := range selectedTasks {
			if err := runTask(ctx, k, bz, cfg, name, rev, c.owner, route, stdout); err != nil {
				return err
			}
		}
		ttl, err := time.ParseDuration(cfg.TTL)
		if err != nil {
			return fmt.Errorf("ttl: %w", err)
		}
		expiry := time.Now().Add(ttl)
		fmt.Fprintf(stdout, "branch=%s owner=%s route=%s base=%s affected=%s\n", branch, c.owner, route, merge, names(ds))
		affected := make(map[string]bool, len(ds))
		resolvedImages := make(map[string]string, len(ds))
		for _, d := range ds {
			affected[d.Name] = true
			tag := route + "-" + shortRevision(rev)
			repository, tagged, err := registry.Destination(cfg.Registry, d.ImageName, tag)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "publishing %s to %s\n", d.Name, tagged)
			if err := bz.PushImage(ctx, d.PushTarget, repository, tag); err != nil {
				return err
			}
			resolved, err := (registry.Resolver{}).Resolve(ctx, tagged)
			if err != nil {
				return err
			}
			resolvedImages[d.Name] = resolved
			fmt.Fprintf(stdout, "resolved %s to %s\n", d.Name, resolved)
		}
		for _, d := range ds {
			if err := k.ApplyOverlay(ctx, cfg, d, resolvedImages[d.Name], c.owner, branch, rev, c.base, merge, route, expiry); err != nil {
				return err
			}
		}
		if err := k.ApplyRoutes(ctx, cfg, allDeployables, affected, c.owner, branch, rev, c.base, merge, route, expiry); err != nil {
			return err
		}
		if err := k.WaitRoutes(ctx, route); err != nil {
			return err
		}
		loop := syncer.Loop{Bazel: bz, Kube: k, Config: cfg, Root: root, Route: route, TTL: ttl, KnownFiles: map[string]map[string]bool{}}
		for _, d := range ds {
			if err := loop.Initial(ctx, d); err != nil {
				_ = k.In(d.WorkloadNamespace(cfg)).RecordSync(ctx, route, d.Name, err)
				return err
			}
			if d.Kind == "frontend" {
				if err := k.In(d.WorkloadNamespace(cfg)).WaitOverlay(ctx, d, route); err != nil {
					return err
				}
			}
			if err := k.In(d.WorkloadNamespace(cfg)).RecordSync(ctx, route, d.Name, nil); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "Open environment: %s\nOpen base: %s\n", openURL, baseURL)
		for _, host := range cfg.PublicHosts {
			if host == cfg.IngressHost {
				continue
			}
			link, _ := deeplink.URL(host, "/", route)
			baseLink, _ := deeplink.URL(host, "/", deeplink.Base)
			fmt.Fprintf(stdout, "Open environment (%s): %s\nOpen base (%s): %s\n", host, link, host, baseLink)
		}
		if noWatch {
			fmt.Fprintln(stdout, "overlays ready; watch disabled")
			return nil
		}
		fmt.Fprintln(stdout, "live sync active; press Ctrl-C to stop local loops")
		return loop.Watch(ctx, ds, func(s string) { fmt.Fprintln(stderr, time.Now().Format(time.RFC3339), s) })
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func findDeployable(ds []config.Deployable, name string) (config.Deployable, bool) {
	for _, d := range ds {
		if d.Name == name {
			return d, true
		}
	}
	return config.Deployable{}, false
}

func validateDeployables(cfg config.Config, ds []config.Deployable) error {
	seen := map[string]bool{}
	for _, d := range ds {
		if d.Name == "" || seen[d.Name] {
			return fmt.Errorf("deployable names must be non-empty and unique: %q", d.Name)
		}
		seen[d.Name] = true
		if !cfg.AllowsNamespace(d.WorkloadNamespace(cfg)) {
			return fmt.Errorf("deployable %s namespace %q is not allowed", d.Name, d.WorkloadNamespace(cfg))
		}
		base := d.Base(cfg)
		if !cfg.AllowsNamespace(base.Namespace) {
			return fmt.Errorf("deployable %s base namespace %q is not allowed", d.Name, base.Namespace)
		}
		if d.Port < 1 || d.OverlayPort() < 1 || base.Port < 1 {
			return fmt.Errorf("deployable %s ports must be positive", d.Name)
		}
		if d.PublicPath != "" && !cfg.AllowsHost(d.Host(cfg)) {
			return fmt.Errorf("deployable %s publicHost %q is not allowed", d.Name, d.Host(cfg))
		}
		proxy := d.Proxy(cfg)
		if proxy.Name != "" {
			if d.PublicPath == "" || d.InternalHost == "" || proxy.Port < 1 || !cfg.AllowsNamespace(proxy.Namespace) {
				return fmt.Errorf("deployable %s publicProxy requires publicPath, internalHost, positive port, and allowed namespace", d.Name)
			}
		}
		if d.WorkloadTemplate != "" && !filepath.IsLocal(d.WorkloadTemplate) {
			return fmt.Errorf("deployable %s workloadTemplate must be repository-relative", d.Name)
		}
		if d.SyncRoot != "" && !filepath.IsAbs(d.SyncRoot) {
			return fmt.Errorf("deployable %s syncRoot must be an absolute container path", d.Name)
		}
		if d.SyncStripPrefix != "" && !filepath.IsLocal(d.SyncStripPrefix) {
			return fmt.Errorf("deployable %s has unsafe syncStripPrefix", d.Name)
		}
	}
	return nil
}

func cleanup(ctx context.Context, k kube.Client, cfg config.Config, now time.Time) (int, error) {
	total := 0
	for _, namespace := range cfg.Namespaces {
		n, err := k.In(namespace).Cleanup(ctx, now)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

type lockedWriter struct {
	mu sync.Mutex
	io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Writer.Write(p)
}

func logs(ctx context.Context, k kube.Client, cfg config.Config, route, service string, stdout, stderr io.Writer) error {
	type ref struct{ namespace, service, container string }
	var refs []ref
	for _, namespace := range cfg.Namespaces {
		list, err := k.In(namespace).List(ctx, kube.ManagedLabel+"=dev-cli,"+kube.RouteLabel+"="+route)
		if err != nil {
			return err
		}
		for _, item := range list.Items {
			name := item.Metadata.Labels[kube.ServiceLabel]
			if service != "" && name != service {
				continue
			}
			container := item.Metadata.Annotations["dev-cli.io/dev-container"]
			if container == "" {
				container = "app"
			}
			refs = append(refs, ref{namespace, name, container})
		}
	}
	if len(refs) == 0 {
		return fmt.Errorf("no owned overlay logs found for service %q", service)
	}
	logCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	out, errOut := &lockedWriter{Writer: stdout}, &lockedWriter{Writer: stderr}
	results := make(chan error, len(refs))
	for _, item := range refs {
		go func(item ref) {
			results <- k.In(item.namespace).Logs(logCtx, route, item.service, item.container, out, errOut)
		}(item)
	}
	for range refs {
		if err := <-results; err != nil {
			return err
		}
	}
	return nil
}

func runTask(ctx context.Context, k kube.Client, bz bazel.Bazel, cfg config.Config, name, revision, owner, route string, w io.Writer) error {
	task, ok := cfg.Task(name)
	if !ok {
		return fmt.Errorf("unknown task %q", name)
	}
	for _, dependencyName := range task.Dependencies {
		dependency, ok := cfg.Dependency(dependencyName)
		if !ok {
			return fmt.Errorf("task %s has unknown dependency %q", name, dependencyName)
		}
		if err := k.Provision(ctx, dependency, owner, route); err != nil {
			return err
		}
	}
	image := ""
	if task.PushTarget != "" {
		repository, tagged, err := registry.Destination(cfg.Registry, task.ImageName, route+"-"+shortRevision(revision))
		if err != nil {
			return err
		}
		if err := bz.PushImage(ctx, task.PushTarget, repository, route+"-"+shortRevision(revision)); err != nil {
			return err
		}
		image, err = (registry.Resolver{}).Resolve(ctx, tagged)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "resolved task %s to %s\n", name, image)
	}
	if err := k.RunTask(ctx, task, cfg.TaskLockNamespace, image, revision, owner, route); err != nil {
		return err
	}
	fmt.Fprintln(w, "completed task", name)
	return nil
}
func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}
func names(ds []config.Deployable) string {
	v := make([]string, len(ds))
	for i := range ds {
		v[i] = ds[i].Name
	}
	return strings.Join(v, ",")
}
func doctor(ctx context.Context, r execx.Runner, c config.Config, base string, w io.Writer) error {
	registryHost := strings.Split(c.Registry, "/")[0]
	checks := []struct {
		name, cmd string
		args      []string
	}{{"git", "git", []string{"merge-base", base, "HEAD"}}, {"bazel", "bazel", []string{"query", c.MetadataQuery, "--output=label", "--noshow_progress"}}, {"kube-context", "kubectl", []string{"--context", c.KubeContext, "cluster-info"}}, {"ingress-class", "kubectl", []string{"--context", c.KubeContext, "get", "ingressclass", c.IngressClass}}, {"gateway-api", "kubectl", []string{"--context", c.KubeContext, "get", "gateway", c.GatewayName, "--namespace", c.GatewayNamespace}}, {"route-rbac", "kubectl", []string{"--context", c.KubeContext, "auth", "can-i", "create", "httproutes.gateway.networking.k8s.io", "--namespace", c.Namespace}}, {"registry", "curl", []string{"-fsS", "-o", "/dev/null", "https://" + registryHost + "/v2/"}}}
	for _, x := range checks {
		if _, err := r.Run(ctx, x.cmd, x.args, nil); err != nil {
			return fmt.Errorf("doctor %s: %w", x.name, err)
		}
		fmt.Fprintln(w, "ok", x.name)
	}
	for _, namespace := range c.Namespaces {
		for _, resource := range []string{"deployments", "services", "jobs.batch", "leases.coordination.k8s.io"} {
			if _, err := r.Run(ctx, "kubectl", []string{"--context", c.KubeContext, "auth", "can-i", "create", resource, "--namespace", namespace}, nil); err != nil {
				return fmt.Errorf("doctor %s %s RBAC: %w", namespace, resource, err)
			}
		}
		fmt.Fprintln(w, "ok namespace", namespace)
	}
	for _, resource := range []string{"backends.gateway.envoyproxy.io", "backendtrafficpolicies.gateway.envoyproxy.io"} {
		if _, err := r.Run(ctx, "kubectl", []string{"--context", c.KubeContext, "--namespace", c.Namespace, "get", resource, "-o", "name"}, nil); err != nil {
			return fmt.Errorf("doctor failover API: %w", err)
		}
		if _, err := r.Run(ctx, "kubectl", []string{"--context", c.KubeContext, "--namespace", c.Namespace, "auth", "can-i", "create", resource}, nil); err != nil {
			return fmt.Errorf("doctor failover RBAC: %w", err)
		}
		fmt.Fprintln(w, "ok", resource)
	}
	return nil
}
func status(ctx context.Context, k kube.Client, cfg config.Config, owner, branch, route string, localRunning bool, w io.Writer) error {
	var v kube.ObjectList
	for _, namespace := range cfg.Namespaces {
		scoped, err := k.In(namespace).List(ctx, kube.ManagedLabel+"=dev-cli,"+kube.RouteLabel+"="+route)
		if err != nil {
			return err
		}
		v.Items = append(v.Items, scoped.Items...)
	}
	routes, err := k.ListRoutes(ctx, kube.ManagedLabel+"=dev-cli,"+kube.RouteLabel+"="+route)
	if err != nil {
		return err
	}
	services := make([]map[string]any, 0, len(v.Items))
	health := "healthy"
	baseRef, baseRevision, sourceRevision, createdAt, expiresAt := "", "", "", "", ""
	for _, item := range v.Items {
		if item.Status.ReadyReplicas < 1 {
			health = "degraded"
		}
		a := item.Metadata.Annotations
		if a["dev-cli.io/sync-health"] != "healthy" {
			health = "degraded"
		}
		if baseRef == "" {
			baseRef, baseRevision, sourceRevision, createdAt, expiresAt = a["dev-cli.io/base-ref"], a["dev-cli.io/base-revision"], a["dev-cli.io/source-revision"], a["dev-cli.io/created-at"], a["dev-cli.io/expires-at"]
		}
		services = append(services, map[string]any{"service": item.Metadata.Labels[kube.ServiceLabel], "workload": item.Metadata.Name, "routing": "overlay", "readyReplicas": item.Status.ReadyReplicas, "syncHealth": a["dev-cli.io/sync-health"], "syncError": a["dev-cli.io/sync-error"], "lastSyncAt": a["dev-cli.io/last-sync-at"]})
	}
	age := "0s"
	routingHealth := "ready"
	if !routes.RoutesReady() {
		routingHealth = "pending-or-degraded"
	}
	if created, err := time.Parse(time.RFC3339, createdAt); err == nil {
		age = time.Since(created).Round(time.Second).String()
	}
	out := map[string]any{"owner": owner, "branch": branch, "route": route, "baseRef": baseRef, "baseRevision": baseRevision, "sourceRevision": sourceRevision, "affectedServices": services, "routingResources": len(routes.Items), "syncHealth": health, "localLoopRunning": localRunning, "age": age, "expiresAt": expiresAt}
	out["routingHealth"] = routingHealth
	urls, baseURLs := map[string]string{}, map[string]string{}
	for _, host := range cfg.PublicHosts {
		urls[host], _ = deeplink.URL(host, "/", route)
		baseURLs[host], _ = deeplink.URL(host, "/", deeplink.Base)
	}
	out["url"], _ = deeplink.URL(cfg.IngressHost, "/", route)
	out["baseURL"], _ = deeplink.URL(cfg.IngressHost, "/", deeplink.Base)
	out["urls"], out["baseURLs"] = urls, baseURLs
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(w, string(b))
	return nil
}
