package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/antonve/dev-cli/internal/bazel"
	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
)

type Loop struct {
	Bazel       bazel.Bazel
	Kube        kube.Client
	Root, Route string
	TTL         time.Duration
	KnownFiles  map[string]map[string]bool
}

func files(root string, roots []string) ([]string, error) {
	var out []string
	for _, r := range roots {
		base := filepath.Join(root, r)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				if errors.Is(e, fs.ErrNotExist) {
					return nil
				}
				return e
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || strings.HasPrefix(d.Name(), "bazel-") {
					return filepath.SkipDir
				}
				return nil
			}
			out = append(out, p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
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
	p, err := l.Kube.Pod(ctx, d, l.Route)
	if err != nil {
		return err
	}
	paths, err := files(l.Root, d.SyncPaths)
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
	if err := l.Kube.SyncFiles(ctx, p, l.Root, relative, known...); err != nil {
		return err
	}
	if dependenciesChanged && len(d.DependencyCommand) > 0 {
		_, err := l.Kube.RunKubectl(ctx, append([]string{"exec", p, "-c", "app", "--"}, d.DependencyCommand...), nil)
		return err
	}
	return nil
}
func (l Loop) syncBackend(ctx context.Context, d config.Deployable) error {
	out, err := l.Bazel.BuildOutput(ctx, d.BuildTarget)
	if err != nil {
		return fmt.Errorf("build failed; last working process preserved: %w", err)
	}
	p, err := l.Kube.Pod(ctx, d, l.Route)
	if err != nil {
		return err
	}
	healthURL := fmt.Sprintf("http://127.0.0.1:%d%s", d.Port, d.ReadinessPath)
	return l.Kube.SyncBinary(ctx, p, out, d.Name, healthURL)
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
		ps, err := files(l.Root, d.SourceRoots)
		if err != nil {
			return err
		}
		h, err := fingerprint(ps)
		if err != nil {
			return err
		}
		_, runtime, err := l.Kube.PodRuntime(ctx, d, l.Route)
		if err != nil {
			return err
		}
		dependencyFiles, err := files(l.Root, d.DependencyPaths)
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
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			if err := l.Kube.Heartbeat(ctx, l.Route, ttl); err != nil {
				report("heartbeat: " + err.Error())
			}
		case <-t.C:
			for i := range states {
				if time.Now().Before(states[i].retryAt) {
					continue
				}
				if time.Now().After(states[i].checkAt) {
					states[i].checkAt = time.Now().Add(2 * time.Second)
					_, runtime, err := l.Kube.PodRuntime(ctx, states[i].d, l.Route)
					if err != nil {
						states[i].hash = ""
						report(states[i].d.Name + ": " + err.Error())
						if stateErr := l.Kube.RecordSync(ctx, l.Route, states[i].d.Name, err); stateErr != nil {
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
				ps, err := files(l.Root, states[i].d.SourceRoots)
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
				dependencyFiles, err := files(l.Root, d.DependencyPaths)
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
					if err := l.Kube.RecordSync(ctx, l.Route, d.Name, syncErr); err != nil {
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
				if err := l.Kube.RecordSync(ctx, l.Route, d.Name, nil); err != nil {
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
