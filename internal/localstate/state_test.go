package localstate

import (
	"os"
	"testing"
)

func TestStateLifecycle(t *testing.T) {
	s := New(t.TempDir(), "route")
	if err := s.Write(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !s.Running() {
		t.Fatal("current process should be running")
	}
	if err := s.Remove(); err != nil {
		t.Fatal(err)
	}
	if s.Running() {
		t.Fatal("removed state should not be running")
	}
}
