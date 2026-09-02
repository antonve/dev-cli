package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
}

func files(root string, roots []string) ([]string, error) {
	var out []string
	for _, r := range roots {
		base := filepath.Join(root, r)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
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
		return l.syncFrontend(ctx, d)
	}
	return nil
}
func (l Loop) syncFrontend(ctx context.Context, d config.Deployable) error {
	p, err := l.Kube.Pod(ctx, d, l.Route)
	if err != nil {
		return err
	}
	for _, rel := range d.SyncPaths {
		local := filepath.Join(l.Root, rel)
		if err := l.Kube.SyncFile(ctx, p, local, "/workspace/"+filepath.ToSlash(rel)); err != nil {
			return err
		}
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
	return l.Kube.SyncBinary(ctx, p, out, d.Name)
}
func (l Loop) Watch(ctx context.Context, ds []config.Deployable, report func(string)) error {
	type state struct {
		d    config.Deployable
		hash string
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
		states = append(states, state{d, h})
	}
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			for i := range states {
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
				var syncErr error
				if d.Kind == "frontend" {
					syncErr = l.syncFrontend(ctx, d)
				} else {
					syncErr = l.syncBackend(ctx, d)
				}
				if syncErr != nil {
					// The error is surfaced once for this exact content. A later
					// edit changes the fingerprint and triggers a fresh attempt.
					states[i].hash = h
					report(d.Name + ": " + syncErr.Error())
					continue
				}
				states[i].hash = h
				report(d.Name + ": synced")
			}
		}
	}
}
