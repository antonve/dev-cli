package localstate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type State struct{ path string }

func New(root, route string) State {
	h := sha256.Sum256([]byte(root + "\x00" + route))
	return State{path: filepath.Join(os.TempDir(), "dev-cli-state", hex.EncodeToString(h[:])[:20]+".pid")}
}

func (s State) Write(pid int) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	return os.WriteFile(s.path, []byte(strconv.Itoa(pid)+"\n"), 0600)
}

func (s State) pid() (int, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return 0, err
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || p < 1 {
		return 0, fmt.Errorf("invalid pid state")
	}
	return p, nil
}
func (s State) Running() bool {
	p, err := s.pid()
	if err != nil {
		return false
	}
	return syscall.Kill(p, 0) == nil
}
func (s State) Stop() (bool, error) {
	p, err := s.pid()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if p == os.Getpid() {
		return false, fmt.Errorf("refusing to signal current process")
	}
	err = syscall.Kill(p, syscall.SIGTERM)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return false, err
	}
	_ = os.Remove(s.path)
	return err == nil, nil
}
func (s State) Remove() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
