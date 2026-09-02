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
	"strings"
	"syscall"
	"time"

	"github.com/antonve/dev-cli/internal/bazel"
	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/execx"
	"github.com/antonve/dev-cli/internal/gitx"
	"github.com/antonve/dev-cli/internal/kube"
	"github.com/antonve/dev-cli/internal/naming"
	"github.com/antonve/dev-cli/internal/syncer"
)

const version = "v0.1.0"

type common struct{ config, owner, base string }

func flags(name string) (*flag.FlagSet, *common) {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &common{}
	f.StringVar(&c.config, "config", ".dev/config.json", "repository config")
	f.StringVar(&c.owner, "owner", "", "owner identity")
	f.StringVar(&c.base, "base", "origin/main", "comparison base")
	return f, c
}
func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: dev <doctor|up|status|logs|down|cleanup|version> [options]")
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
	if args[0] == "up" {
		f.BoolVar(&noWatch, "no-watch", false, "create and initially sync overlays, then exit")
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
	k := kube.Client{Run: r, Context: cfg.KubeContext, Namespace: cfg.Namespace}
	bz := bazel.Bazel{Run: r}
	switch args[0] {
	case "doctor":
		return doctor(ctx, r, cfg, c.base, stdout)
	case "cleanup":
		n, err := k.Cleanup(ctx, time.Now())
		fmt.Fprintf(stdout, "removed %d expired route(s)\n", n)
		return err
	case "down":
		if err := k.Down(ctx, c.owner, route); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed route %s\n", route)
		return nil
	case "logs":
		return k.Logs(ctx, route, service, stdout, stderr)
	case "status":
		_, _ = k.Cleanup(ctx, time.Now())
		return status(ctx, k, c.owner, branch, route, stdout)
	case "up":
		_, _ = k.Cleanup(ctx, time.Now())
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
		fmt.Fprintf(stdout, "branch=%s owner=%s route=%s base=%s affected=%s\n", branch, c.owner, route, merge, names(ds))
		for _, d := range ds {
			if err := k.ApplyOverlay(ctx, cfg, d, c.owner, branch, rev, c.base, merge, route, expiry); err != nil {
				return err
			}
		}
		loop := syncer.Loop{Bazel: bz, Kube: k, Root: root, Route: route}
		for _, d := range ds {
			if err := loop.Initial(ctx, d); err != nil {
				return err
			}
		}
		if noWatch {
			fmt.Fprintln(stdout, "overlays ready; watch disabled")
			return nil
		}
		watchCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		fmt.Fprintln(stdout, "live sync active; press Ctrl-C to stop local loops")
		return loop.Watch(watchCtx, ds, func(s string) { fmt.Fprintln(stderr, time.Now().Format(time.RFC3339), s) })
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}
func names(ds []config.Deployable) string {
	v := make([]string, len(ds))
	for i := range ds {
		v[i] = ds[i].Name
	}
	return strings.Join(v, ",")
}
func doctor(ctx context.Context, r execx.Runner, c config.Config, base string, w io.Writer) error {
	checks := []struct {
		name, cmd string
		args      []string
	}{{"git", "git", []string{"merge-base", base, "HEAD"}}, {"bazel", "bazel", []string{"query", c.MetadataQuery, "--output=label", "--noshow_progress"}}, {"kube-context", "kubectl", []string{"--context", c.KubeContext, "cluster-info"}}, {"namespace-rbac", "kubectl", []string{"--context", c.KubeContext, "auth", "can-i", "create", "deployments", "--namespace", c.Namespace}}, {"ingress-class", "kubectl", []string{"--context", c.KubeContext, "get", "ingressclass", c.IngressClass}}, {"registry", "curl", []string{"-fsS", "-o", "/dev/null", "https://" + c.Registry + "/v2/"}}}
	for _, x := range checks {
		if _, err := r.Run(ctx, x.cmd, x.args, nil); err != nil {
			return fmt.Errorf("doctor %s: %w", x.name, err)
		}
		fmt.Fprintln(w, "ok", x.name)
	}
	return nil
}
func status(ctx context.Context, k kube.Client, owner, branch, route string, w io.Writer) error {
	v, err := k.List(ctx, kube.ManagedLabel+"=dev-cli,"+kube.RouteLabel+"="+route)
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
		if baseRef == "" {
			baseRef, baseRevision, sourceRevision, createdAt, expiresAt = a["dev-cli.io/base-ref"], a["dev-cli.io/base-revision"], a["dev-cli.io/source-revision"], a["dev-cli.io/created-at"], a["dev-cli.io/expires-at"]
		}
		services = append(services, map[string]any{"service": item.Metadata.Labels[kube.ServiceLabel], "workload": item.Metadata.Name, "routing": "overlay", "readyReplicas": item.Status.ReadyReplicas})
	}
	age := "0s"
	if created, err := time.Parse(time.RFC3339, createdAt); err == nil {
		age = time.Since(created).Round(time.Second).String()
	}
	out := map[string]any{"owner": owner, "branch": branch, "route": route, "baseRef": baseRef, "baseRevision": baseRevision, "sourceRevision": sourceRevision, "affectedServices": services, "syncHealth": health, "age": age, "expiresAt": expiresAt}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(w, string(b))
	return nil
}
