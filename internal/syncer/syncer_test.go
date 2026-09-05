package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antonve/dev-cli/internal/config"
	"github.com/antonve/dev-cli/internal/kube"
)

func TestFingerprintChanges(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "app.tsx")
	if err := os.WriteFile(p, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := fingerprint([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := fingerprint([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("fingerprint did not change")
	}
}

type recoveryRunner struct {
	queries, transfers int
	installs           int
	path               string
	replace, failCopy  bool
}

func (r *recoveryRunner) Run(_ context.Context, _ string, args []string, _ io.Reader) ([]byte, error) {
	command := strings.Join(args, " ")
	if strings.Contains(command, "install-frozen") {
		r.installs++
	}
	if strings.Contains(command, "get pods") {
		r.queries++
		if r.queries == 2 && !r.replace {
			if err := os.WriteFile(r.path, []byte("edited"), 0600); err != nil {
				return nil, err
			}
		}
		uid := "first"
		if r.replace && r.queries >= 2 {
			uid = "replacement"
		}
		return []byte(fmt.Sprintf(`{"items":[{"metadata":{"name":"pod","uid":%q},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"app","restartCount":0}]}}]}`, uid)), nil
	}
	if strings.Contains(command, "tar -x") {
		r.transfers++
		if r.failCopy && r.transfers == 1 {
			return nil, errors.New("temporary transfer interruption")
		}
	}
	return nil, nil
}
func (*recoveryRunner) Stream(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

func TestWatchRecoversWithoutAnotherEdit(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%v", replace), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "app.ts")
			if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			r := &recoveryRunner{path: path, replace: replace, failCopy: !replace}
			loop := Loop{Kube: kube.Client{Run: r, Context: "dev", Namespace: "ns"}, Root: root, Route: "test"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			synced := false
			err := loop.Watch(ctx, []config.Deployable{{Name: "web", Kind: "frontend", SourceRoots: []string{"app.ts"}, SyncPaths: []string{"app.ts"}}}, func(message string) {
				if message == "web: synced" {
					synced = true
					cancel()
				}
			})
			if err != nil || !synced {
				t.Fatalf("no recovery: transfers=%d error=%v", r.transfers, err)
			}
			if !replace && r.transfers != 2 {
				t.Fatalf("expected failed and retried transfer: %d", r.transfers)
			}
		})
	}
}

func TestDependencyCommandOnlyRunsWhenRequested(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.ts")
	if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &recoveryRunner{path: path}
	l := Loop{Kube: kube.Client{Run: r}, Root: root, Route: "route", KnownFiles: map[string]map[string]bool{}}
	d := config.Deployable{Name: "web", Kind: "frontend", SyncPaths: []string{"app.ts"}, DependencyCommand: []string{"install-frozen"}}
	for _, changed := range []bool{true, false, false, true} {
		if err := l.syncFrontend(context.Background(), d, changed); err != nil {
			t.Fatal(err)
		}
	}
	if r.installs != 2 || r.transfers != 4 {
		t.Fatalf("installs=%d transfers=%d", r.installs, r.transfers)
	}
	if !l.KnownFiles["web"]["app.ts"] {
		t.Fatal("missing recovery manifest")
	}
}

func TestRetryBackoffIsBounded(t *testing.T) {
	for attempts, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 5: 16 * time.Second, 100: 16 * time.Second} {
		if got := retryDelay(attempts); got != want {
			t.Fatalf("retry %d: %v != %v", attempts, got, want)
		}
	}
}
