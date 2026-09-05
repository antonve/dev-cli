package localstate

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStateLifecycleRejectsDuplicateAndWaitsForStop(t *testing.T) {
	s := State{path: filepath.Join(t.TempDir(), "private", "owner.sock")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := s.Start(cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !s.Running() {
		t.Fatal("owner not running")
	}
	if duplicate, err := s.Start(func() {}); err == nil {
		duplicate()
		t.Fatal("duplicate owner acquired lock")
	}
	result := make(chan error, 1)
	go func() {
		stopped, err := s.Stop()
		if err == nil && !stopped {
			err = fmt.Errorf("owner not stopped")
		}
		result <- err
	}()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not cancel owner")
	}
	select {
	case err := <-result:
		t.Fatalf("stop returned before owner exited: %v", err)
	default:
	}
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if s.Running() {
		t.Fatal("closed owner still running")
	}
	if stopped, err := s.Stop(); err != nil || stopped {
		t.Fatalf("absent stop: %v %v", stopped, err)
	}
}

func TestAbruptExitReleasesLockAndRecoversSocket(t *testing.T) {
	s := State{path: filepath.Join(t.TempDir(), "private", "owner.sock")}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$")
	cmd.Env = append(os.Environ(), "DEV_CLI_TEST_SOCKET="+s.path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper: %q %v", line, err)
	}
	if !s.Running() {
		t.Fatal("helper did not acquire owner")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if s.Running() {
		t.Fatal("dead process still running")
	}
	release, err := s.Start(func() {})
	if err != nil {
		t.Fatalf("stale socket recovery: %v", err)
	}
	defer release()
	if !s.Running() {
		t.Fatal("new owner not running")
	}
}

func TestCrashHelper(t *testing.T) {
	path := os.Getenv("DEV_CLI_TEST_SOCKET")
	if path == "" {
		return
	}
	release, err := (State{path: path}).Start(func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fmt.Println("ready")
	select {}
}
