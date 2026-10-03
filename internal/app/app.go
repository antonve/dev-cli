package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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

const version = kube.CLIVersion

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
	return run(ctx, args, stdout, stderr, execx.OS{})
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, r execx.Runner) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("command required")
	}
	if args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return nil
	}
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
	cookieSelection := false
	urlHost := ""
	var selectedServices, selectedDependencies, selectedTasks stringsFlag
	if args[0] == "url" {
		f.BoolVar(&clearSelection, "clear", false, "link to base and clear the branch cookie")
		f.BoolVar(&cookieSelection, "cookie", false, "use a base-host cookie selection link")
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
		if cfg.ClusterIssuer != "" && !clearSelection && !cookieSelection {
			link, err = deeplink.HostURL(urlHost, route, f.Arg(0))
		}
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
		n, err := cleanup(ctx, k, cfg, time.Now(), true, stderr)
		fmt.Fprintf(stdout, "removed %d expired route(s)\n", n)
		return err
	case "down":
		stopped, err := local.Stop()
		if err != nil {
			return fmt.Errorf("stop local loop: %w", err)
		}
		if err := teardown(ctx, k, cfg, c.owner, route, stdout); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed route %s local-loop-stopped=%t\n", route, stopped)
		return nil
	case "logs":
		return logs(ctx, k, cfg, route, service, stdout, stderr)
	case "status":
		_, _ = cleanup(ctx, k, cfg, time.Now(), false, stderr)
		return status(ctx, k, cfg, c.owner, branch, route, local.Running(), stdout)
	case "provision":
		cfg, _, k, err = resolveProfileContext(ctx, g, k, cfg, route, c.base)
		if err != nil {
			return err
		}
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
		cfg, _, k, err = resolveProfileContext(ctx, g, k, cfg, route, c.base)
		if err != nil {
			return err
		}
		if f.NArg() != 1 {
			return fmt.Errorf("usage: dev task [flags] name")
		}
		rev, err := g.Revision(ctx)
		if err != nil {
			return err
		}
		return runTask(ctx, k, bz, cfg, f.Arg(0), rev, c.owner, route, stdout)
	case "up":
		openURL, baseURL := "", ""
		if cfg.IngressHost != "" {
			openURL, err = deeplink.URL(cfg.IngressHost, "/", route)
			if err != nil {
				return err
			}
			baseURL, _ = deeplink.URL(cfg.IngressHost, "/", deeplink.Base)
		}
		upCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		release, err := local.Start(stop)
		if err != nil {
			return err
		}
		defer release()
		ctx = upCtx
		if _, err := cleanup(ctx, k, cfg, time.Now(), true, stderr, route); err != nil {
			return err
		}
		merge, err := g.MergeBase(ctx, c.base)
		if err != nil {
			return err
		}
		changed, err := g.Changed(ctx, merge)
		if err != nil {
			return err
		}
		cfg, profile := cfg.ForPaths(changed)
		marker, err := k.ReadLifecycle(ctx, route)
		if err != nil {
			return err
		}
		if marker != nil && marker.Profile != profile {
			return fmt.Errorf("route %s runs profile %s; run dev down first", route, marker.Profile)
		}
		resolvedVariables := cfg.ResolveVariables(route, cfg.Namespace)
		if marker != nil && !maps.Equal(marker.Variables, resolvedVariables) {
			return fmt.Errorf("route %s has different recorded variables; run dev down first", route)
		}
		k.Variables = resolvedVariables
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
		for _, name := range selectedServices {
			if _, ok := findDeployable(allDeployables, name); !ok {
				return fmt.Errorf("unknown service %q", name)
			}
		}
		ds = selectDeployables(allDeployables, ds, selectedServices)
		if len(ds) == 0 {
			fmt.Fprintln(stdout, "no affected deployables")
			return nil
		}
		rev, err := g.Revision(ctx)
		if err != nil {
			return err
		}
		ttl, err := time.ParseDuration(cfg.TTL)
		if err != nil {
			return fmt.Errorf("ttl: %w", err)
		}
		expiry := time.Now().Add(ttl)
		deployableNames := make([]string, 0, len(ds))
		for _, d := range ds {
			deployableNames = append(deployableNames, d.Name)
		}
		if err := k.WriteLifecycle(ctx, kube.Lifecycle{Owner: c.owner, Checkout: root, Profile: profile, Variables: k.Variables, Hooks: cfg.Hooks, Deployables: deployableNames}, branch, rev, c.base, merge, route, expiry); err != nil {
			return err
		}
		loop := syncer.Loop{Bazel: bz, Kube: k, Config: cfg, Root: root, Route: route, TTL: ttl, KnownFiles: map[string]map[string]bool{}}
		stopHeartbeat, heartbeatErrors := loop.StartHeartbeat(ctx, ds)
		defer stopHeartbeat()
		loop.HeartbeatErrors = heartbeatErrors
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
		if err := runHooks(ctx, k, bz, cfg, cfg.Hooks.BeforeUp, rev, c.owner, route, stdout); err != nil {
			return err
		}
		for _, name := range selectedTasks {
			if err := runTask(ctx, k, bz, cfg, name, rev, c.owner, route, stdout); err != nil {
				return err
			}
		}
		fmt.Fprintf(stdout, "branch=%s owner=%s route=%s profile=%s base=%s affected=%s\n", branch, c.owner, route, profile, merge, names(ds))
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
			if err := runHooks(ctx, k, bz, cfg, cfg.Hooks.Deployables[d.Name].BeforeStart, rev, c.owner, route, stdout); err != nil {
				return err
			}
			if err := k.ApplyOverlay(ctx, cfg, d, resolvedImages[d.Name], c.owner, branch, rev, c.base, merge, route, expiry); err != nil {
				return err
			}
		}
		if err := k.ApplyRoutes(ctx, cfg, allDeployables, affected, c.owner, branch, rev, c.base, merge, route, expiry); err != nil {
			return err
		}
		if hasRoutableDeployable(allDeployables) {
			if err := k.WaitRoutes(ctx, route); err != nil {
				return err
			}
			if cfg.ClusterIssuer != "" {
				name := naming.Resource("tls", route)
				if err := k.WaitCertificate(ctx, name); err != nil {
					fmt.Fprintf(stderr, "warning: branch certificate not ready; inspect kubectl --context %s -n %s describe certificate %s; cookie links remain available: %v\n", cfg.KubeContext, cfg.Namespace, name, err)
				}
			}
		}
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
		if cfg.IngressHost != "" {
			if cfg.ClusterIssuer != "" {
				for _, host := range cfg.PublicHosts {
					link, _ := deeplink.HostURL(host, route, "/")
					fmt.Fprintf(stdout, "Open branch host (%s): %s\n", host, link)
				}
				fmt.Fprintln(stdout, "Cookie fallback links:")
			}
			fmt.Fprintf(stdout, "Open environment: %s\nOpen base: %s\n", openURL, baseURL)
		}
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

func selectDeployables(all, affected []config.Deployable, explicit []string) []config.Deployable {
	selected, groups := map[string]bool{}, map[string]bool{}
	for _, d := range affected {
		selected[d.Name] = true
	}
	for _, name := range explicit {
		selected[name] = true
	}
	for _, d := range all {
		if selected[d.Name] && d.SelectionGroup != "" {
			groups[d.SelectionGroup] = true
		}
	}
	var result []config.Deployable
	for _, d := range all {
		if selected[d.Name] || d.SelectionGroup != "" && groups[d.SelectionGroup] {
			result = append(result, d)
		}
	}
	return result
}

func hasRoutableDeployable(ds []config.Deployable) bool {
	for _, d := range ds {
		if d.Kind != "worker" && (d.PublicPath != "" || d.InternalHost != "") {
			return true
		}
	}
	return false
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
		if d.Kind == "worker" {
			if d.ContainerPath == "" || (d.Port > 0) != (d.ReadinessPath != "") || d.Port < 0 || d.ServicePort != 0 || d.PublicPath != "" || d.InternalHost != "" || d.PublicHost != "" || d.BaseService != (config.ObjectRef{}) || d.PublicProxy != (config.ObjectRef{}) {
				return fmt.Errorf("deployable %s worker requires containerPath, an optional private port/readinessPath pair, and no Service or routes", d.Name)
			}
		} else {
			base := d.Base(cfg)
			if !cfg.AllowsNamespace(base.Namespace) {
				return fmt.Errorf("deployable %s base namespace %q is not allowed", d.Name, base.Namespace)
			}
			if d.Port < 1 || d.OverlayPort() < 1 || base.Port < 1 {
				return fmt.Errorf("deployable %s ports must be positive", d.Name)
			}
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
	hooks := []config.Hooks{cfg.Hooks}
	for _, profile := range cfg.Profiles {
		hooks = append(hooks, profile.Hooks)
	}
	for _, h := range hooks {
		for name := range h.Deployables {
			if !seen[name] {
				return fmt.Errorf("hook names unknown deployable %q", name)
			}
		}
	}
	return nil
}

func resolveProfileContext(ctx context.Context, g gitx.Git, k kube.Client, cfg config.Config, route, base string) (config.Config, string, kube.Client, error) {
	marker, err := k.ReadLifecycle(ctx, route)
	if err != nil {
		return cfg, "", k, err
	}
	if marker != nil {
		effective, err := cfg.ForProfile(marker.Profile)
		k.Variables = marker.Variables
		return effective, marker.Profile, k, err
	}
	merge, err := g.MergeBase(ctx, base)
	if err != nil {
		return cfg, "", k, err
	}
	changed, err := g.Changed(ctx, merge)
	if err != nil {
		return cfg, "", k, err
	}
	effective, profile := cfg.ForPaths(changed)
	k.Variables = effective.ResolveVariables(route, cfg.Namespace)
	return effective, profile, k, nil
}

func runHooks(ctx context.Context, k kube.Client, bz bazel.Bazel, cfg config.Config, tasks []string, revision, owner, route string, w io.Writer) error {
	for _, name := range tasks {
		if err := runTask(ctx, k, bz, cfg, name, revision, owner, route, w); err != nil {
			return err
		}
	}
	return nil
}

func teardown(ctx context.Context, k kube.Client, cfg config.Config, owner, route string, w io.Writer) error {
	marker, err := k.ReadLifecycle(ctx, route)
	if err != nil {
		return err
	}
	if marker != nil {
		k.Variables = marker.Variables
		if owner != marker.Owner {
			return fmt.Errorf("route %s belongs to %s", route, marker.Owner)
		}
		for _, name := range marker.Hooks.TaskNames() {
			if _, ok := cfg.Task(name); !ok {
				return fmt.Errorf("route %s recorded task %q is missing from config", route, name)
			}
		}
	}
	stopped := map[string]bool{}
	if marker == nil || !marker.OverlaysStopped {
		if marker != nil {
			for _, name := range marker.Deployables {
				stopped[name] = true
			}
		}
		for _, namespace := range cfg.Namespaces {
			list, err := k.In(namespace).List(ctx, kube.ManagedLabel+"=dev-cli,"+kube.OwnerLabel+"="+naming.Slug(owner, 40)+","+kube.RouteLabel+"="+route)
			if err != nil {
				return err
			}
			for _, item := range list.Items {
				if name := item.Metadata.Labels[kube.ServiceLabel]; name != "" {
					stopped[name] = true
				}
			}
		}
		services := make([]string, 0, len(stopped))
		for name := range stopped {
			services = append(services, name)
		}
		sort.Strings(services)
		if marker != nil {
			marker.Deployables = services
			if err := k.SaveLifecycle(ctx, route, *marker); err != nil {
				return err
			}
		}
		for _, namespace := range cfg.Namespaces {
			if err := k.In(namespace).Down(ctx, owner, route); err != nil {
				return err
			}
		}
		for _, name := range services {
			for _, namespace := range cfg.Namespaces {
				if err := k.In(namespace).WaitStopped(ctx, route, name); err != nil {
					return err
				}
			}
		}
		if marker != nil {
			marker.Deployables = services
			marker.OverlaysStopped = true
			if err := k.SaveLifecycle(ctx, route, *marker); err != nil {
				return err
			}
		}
	}
	if marker != nil {
		bz := bazel.Bazel{Run: k.Run, Args: cfg.BazelArgs}
		for _, name := range marker.Deployables {
			if err := runHooks(ctx, k, bz, cfg, marker.Hooks.Deployables[name].AfterStop, marker.Revision, owner, route, w); err != nil {
				return err
			}
		}
		if err := runHooks(ctx, k, bz, cfg, marker.Hooks.AfterDown, marker.Revision, owner, route, w); err != nil {
			return err
		}
	}
	for _, dependency := range cfg.Dependencies {
		if err := k.RemoveDependency(ctx, dependency, owner, route); err != nil {
			return err
		}
	}
	for _, namespace := range cfg.Namespaces {
		if err := k.In(namespace).DeleteTasks(ctx, owner, route); err != nil {
			return err
		}
	}
	if marker != nil {
		return k.DeleteLifecycle(ctx, owner, route)
	}
	return nil
}

func cleanup(ctx context.Context, k kube.Client, cfg config.Config, now time.Time, hooks bool, w io.Writer, protectedRoute ...string) (int, error) {
	expiries := map[string]time.Time{}
	for _, namespace := range cfg.Namespaces {
		routes, err := k.In(namespace).Cleanup(ctx, now)
		if err != nil {
			return 0, err
		}
		for route, expiry := range routes {
			if expiry.After(expiries[route]) {
				expiries[route] = expiry
			}
		}
	}
	routes := make([]string, 0, len(expiries))
	for route, expiry := range expiries {
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
		if marker != nil {
			if !hooks {
				continue
			}
			missing := ""
			for _, name := range marker.Hooks.TaskNames() {
				if _, ok := cfg.Task(name); !ok {
					missing = name
					break
				}
			}
			if missing != "" {
				fmt.Fprintf(w, "warning: route %s teardown skipped: recorded task %q is missing from config\n", route, missing)
				continue
			}
			if marker.Checkout != "" {
				root, err := os.Getwd()
				if err != nil {
					return removed, err
				}
				protected := len(protectedRoute) > 0 && protectedRoute[0] == route && marker.Checkout == root
				if !protected {
					if _, err := localstate.New(marker.Checkout, route).Stop(); err != nil {
						return removed, fmt.Errorf("stop local loop for route %s: %w", route, err)
					}
				}
			}
			if err := teardown(ctx, k, cfg, marker.Owner, route, w); err != nil {
				return removed, err
			}
		} else {
			for _, namespace := range cfg.Namespaces {
				sel := kube.ManagedLabel + "=dev-cli," + kube.RouteLabel + "=" + route
				if _, err := k.In(namespace).RunKubectl(ctx, []string{"delete", "deployment,service,configmap,httproute,backends.gateway.envoyproxy.io,backendtrafficpolicies.gateway.envoyproxy.io", "-l", sel, "--ignore-not-found=true", "--cascade=foreground", "--wait=true", "--timeout=120s"}, nil); err != nil {
					return removed, err
				}
			}
		}
		removed++
	}
	return removed, nil
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
	if c.ClusterIssuer != "" {
		for _, args := range [][]string{
			{"auth", "can-i", "create", "ingresses.networking.k8s.io", "--namespace", c.Namespace},
			{"auth", "can-i", "get", "certificates.cert-manager.io", "--namespace", c.Namespace},
			{"get", "service", "dev-cli-gateway", "--namespace", c.Namespace},
			{"get", "clusterissuer", c.ClusterIssuer},
		} {
			if _, err := r.Run(ctx, "kubectl", append([]string{"--context", c.KubeContext}, args...), nil); err != nil {
				return fmt.Errorf("doctor branch hosts: %w", err)
			}
		}
		fmt.Fprintln(w, "ok branch hosts")
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
	workerOnly := len(v.Items) > 0
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
		routing := "overlay"
		if a["dev-cli.io/kind"] == "worker" {
			routing = "none"
		} else {
			workerOnly = false
		}
		services = append(services, map[string]any{"service": item.Metadata.Labels[kube.ServiceLabel], "kind": a["dev-cli.io/kind"], "workload": item.Metadata.Name, "routing": routing, "readyReplicas": item.Status.ReadyReplicas, "syncHealth": a["dev-cli.io/sync-health"], "syncError": a["dev-cli.io/sync-error"], "lastSyncAt": a["dev-cli.io/last-sync-at"]})
	}
	age := "0s"
	routingHealth := "ready"
	if len(routes.Items) == 0 && workerOnly {
		routingHealth = "none"
	} else if !routes.RoutesReady() {
		routingHealth = "pending-or-degraded"
	}
	if created, err := time.Parse(time.RFC3339, createdAt); err == nil {
		age = time.Since(created).Round(time.Second).String()
	}
	out := map[string]any{"owner": owner, "branch": branch, "route": route, "baseRef": baseRef, "baseRevision": baseRevision, "sourceRevision": sourceRevision, "affectedServices": services, "routingResources": len(routes.Items), "syncHealth": health, "localLoopRunning": localRunning, "age": age, "expiresAt": expiresAt}
	marker, err := k.ReadLifecycle(ctx, route)
	if err != nil {
		return err
	}
	out["profile"] = "default"
	if marker != nil {
		out["profile"] = marker.Profile
	}
	out["teardownPending"] = marker != nil && (marker.OverlaysStopped || time.Now().After(marker.Expiry))
	out["routingHealth"] = routingHealth
	urls, baseURLs := map[string]string{}, map[string]string{}
	for _, host := range cfg.PublicHosts {
		urls[host], _ = deeplink.URL(host, "/", route)
		baseURLs[host], _ = deeplink.URL(host, "/", deeplink.Base)
	}
	out["url"], _ = deeplink.URL(cfg.IngressHost, "/", route)
	out["baseURL"], _ = deeplink.URL(cfg.IngressHost, "/", deeplink.Base)
	out["urls"], out["baseURLs"] = urls, baseURLs
	if cfg.ClusterIssuer != "" {
		out["cookieURL"], out["cookieURLs"] = out["url"], urls
		hostURLs := map[string]string{}
		for _, host := range cfg.PublicHosts {
			hostURLs[host], _ = deeplink.HostURL(host, route, "/")
		}
		out["url"], _ = deeplink.HostURL(cfg.IngressHost, route, "/")
		out["urls"] = hostURLs
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(w, string(b))
	return nil
}
