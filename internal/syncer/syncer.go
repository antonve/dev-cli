package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/bazel"
	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/execx"
	"github.com/antonve/dev-cli/internal/kube"
)

type Loop struct {
	Bazel       bazel.Bazel
	Kube        kube.Client
	Config      config.Config
	Root, Route string
	TTL         time.Duration
	KnownFiles  map[string]map[string]bool
}

func (l Loop) podRuntime(ctx context.Context, d config.Deployable) (string, string, error) {
	k := l.Kube.In(d.WorkloadNamespace(l.Config))
	if d.Kind == "frontend" {
		return k.RunningPodRuntime(ctx, d, l.Route)
	}
	return k.PodRuntime(ctx, d, l.Route)
}

func files(r execx.Runner, root string, roots, excludes []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	args := []string{"-C", root, "ls-files", "-co", "--exclude-standard", "-z", "--"}
	for _, path := range roots {
		if !filepath.IsLocal(path) {
			return nil, fmt.Errorf("unsafe source root %q", path)
		}
		args = append(args, filepath.ToSlash(path))
	}
	b, err := r.Run(context.Background(), "git", args, nil)
	if err != nil {
		return nil, fmt.Errorf("list safe sync files: %w", err)
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, raw := range strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00") {
		if raw == "" || excluded(raw, excludes) {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(raw))
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(physicalRoot, resolved)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("sync file escapes repository: %s", raw)
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func excluded(path string, patterns []string) bool {
	path = filepath.ToSlash(path)
	for _, part := range strings.Split(path, "/") {
		if part == ".git" || part == "node_modules" || part == ".next" || part == "dist" || part == "coverage" || strings.HasPrefix(part, "bazel-") || part == ".env" || strings.HasPrefix(part, ".env.") {
			return true
		}
	}
	for _, pattern := range patterns {
		if ok, _ := filepath.Match(pattern, path); ok {
			return true
		}
	}
	return false
}
func fingerprint(paths []string) (string, error) {
	h := sha256.New()
	for _, p := range paths {
		b, e := kube.ReadFile(p)
		if e != nil {
			return "", e
		}
		h.Write([]byte(p))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (l Loop) Initial(ctx context.Context, d config.Deployable) error {
	if d.Kind == "frontend" {
		return l.syncFrontend(ctx, d, true)
	}
	return nil
}
func (l Loop) syncFrontend(ctx context.Context, d config.Deployable, dependenciesChanged bool) error {
	k := l.Kube.In(d.WorkloadNamespace(l.Config))
	p, err := k.RunningPod(ctx, d, l.Route)
	if err != nil {
		return err
	}
	paths, err := files(l.Bazel.Run, l.Root, d.SyncPaths, d.SyncExcludes)
	if err != nil {
		return err
	}
	relative := make([]string, 0, len(paths))
	for _, path := range paths {
		rel, err := filepath.Rel(l.Root, path)
		if err != nil {
			return err
		}
		relative = append(relative, rel)
	}
	var known []string
	for path := range l.KnownFiles[d.Name] {
		known = append(known, path)
	}
	if l.KnownFiles != nil {
		if l.KnownFiles[d.Name] == nil {
			l.KnownFiles[d.Name] = map[string]bool{}
		}
		for _, path := range relative {
			l.KnownFiles[d.Name][path] = true
		}
	}
	if err := k.SyncFiles(ctx, p, d, l.Root, relative, known...); err != nil {
		return err
	}
	if dependenciesChanged && len(d.DependencyCommand) > 0 {
		_, err := k.RunKubectl(ctx, append([]string{"exec", p, "-c", d.Container(), "--"}, d.DependencyCommand...), nil)
		return err
	}
	return nil
}
func (l Loop) syncBackend(ctx context.Context, d config.Deployable) error {
	out, err := l.Bazel.BuildOutput(ctx, d.BuildTarget)
	if err != nil {
		return fmt.Errorf("build failed; last working process preserved: %w", err)
	}
	k := l.Kube.In(d.WorkloadNamespace(l.Config))
	p, err := k.Pod(ctx, d, l.Route)
	if err != nil {
		return err
	}
	healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", d.Port, d.ReadinessPath)
	return k.SyncBinary(ctx, p, d.Container(), out, d.Name, healthURL)
}
func (l Loop) Watch(ctx context.Context, ds []config.Deployable, report func(string)) error {
	type state struct {
		d                config.Deployable
		hash             string
		runtime          string
		dependencyHash   string
		checkAt, retryAt time.Time
		failures         int
	}
	states := make([]state, 0, len(ds))
	for _, d := range ds {
		ps, err := files(l.Bazel.Run, l.Root, d.SourceRoots, d.SyncExcludes)
		if err != nil {
			return err
		}
		h, err := fingerprint(ps)
		if err != nil {
			return err
		}
		_, runtime, err := l.podRuntime(ctx, d)
		if err != nil {
			return err
		}
		dependencyFiles, err := files(l.Bazel.Run, l.Root, d.DependencyPaths, d.SyncExcludes)
		if err != nil {
			return err
		}
		dependencyHash, err := fingerprint(dependencyFiles)
		if err != nil {
			return err
		}
		states = append(states, state{d: d, hash: h, runtime: runtime, dependencyHash: dependencyHash})
	}
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	ttl := l.TTL
	if ttl <= 0 {
		ttl = 8 * time.Hour
	}
	heartbeat := time.NewTicker(min(30*time.Second, max(ttl/3, time.Second)))
	defer heartbeat.Stop()
	heartbeatNamespaces := map[string]bool{l.Config.Namespace: true}
	for _, state := range states {
		heartbeatNamespaces[state.d.WorkloadNamespace(l.Config)] = true
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			for namespace := range heartbeatNamespaces {
				if err := l.Kube.In(namespace).Heartbeat(ctx, l.Route, ttl); err != nil {
					report("heartbeat: " + err.Error())
				}
			}
		case <-t.C:
			for i := range states {
				if time.Now().Before(states[i].retryAt) {
					continue
				}
				if time.Now().After(states[i].checkAt) {
					states[i].checkAt = time.Now().Add(2 * time.Second)
					_, runtime, err := l.podRuntime(ctx, states[i].d)
					if err != nil {
						states[i].hash = ""
						report(states[i].d.Name + ": " + err.Error())
						if stateErr := l.Kube.In(states[i].d.WorkloadNamespace(l.Config)).RecordSync(ctx, l.Route, states[i].d.Name, err); stateErr != nil {
							report(stateErr.Error())
						}
						states[i].retryAt = time.Now().Add(2 * time.Second)
						continue
					}
					if runtime != states[i].runtime {
						states[i].hash = ""
						states[i].dependencyHash = ""
						states[i].runtime = runtime
					}
				}
				ps, err := files(l.Bazel.Run, l.Root, states[i].d.SourceRoots, states[i].d.SyncExcludes)
				if err != nil {
					report(err.Error())
					continue
				}
				h, err := fingerprint(ps)
				if err != nil {
					report(err.Error())
					continue
				}
				if h == states[i].hash {
					continue
				}
				d := states[i].d
				dependencyFiles, err := files(l.Bazel.Run, l.Root, d.DependencyPaths, d.SyncExcludes)
				if err != nil {
					report(err.Error())
					continue
				}
				dependencyHash, err := fingerprint(dependencyFiles)
				if err != nil {
					report(err.Error())
					continue
				}
				var syncErr error
				if d.Kind == "frontend" {
					syncErr = l.syncFrontend(ctx, d, dependencyHash != states[i].dependencyHash)
				} else {
					syncErr = l.syncBackend(ctx, d)
				}
				if syncErr != nil {
					if err := l.Kube.In(d.WorkloadNamespace(l.Config)).RecordSync(ctx, l.Route, d.Name, syncErr); err != nil {
						report(err.Error())
					}
					states[i].failures++
					states[i].retryAt = time.Now().Add(retryDelay(states[i].failures))
					report(d.Name + ": " + syncErr.Error())
					continue
				}
				states[i].hash = h
				states[i].dependencyHash = dependencyHash
				states[i].failures = 0
				if err := l.Kube.In(d.WorkloadNamespace(l.Config)).RecordSync(ctx, l.Route, d.Name, nil); err != nil {
					report(err.Error())
				}
				report(d.Name + ": synced")
			}
		}
	}
}

func retryDelay(failures int) time.Duration {
	return time.Second * time.Duration(1<<min(max(failures-1, 0), 4))
}
