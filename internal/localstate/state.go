package localstate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type State struct{ path string }

func New(root, route string) State {
	h := sha256.Sum256([]byte(root + "\x00" + route))
	return State{path: filepath.Join(os.TempDir(), fmt.Sprintf("dev-cli-%d", os.Getuid()), hex.EncodeToString(h[:])[:20]+".sock")}
}

// Start takes a process-lifetime lock before any cluster mutation. A socket
// controls the actual owner; no stored PID can ever signal a reused process.
func (s State) Start(cancel func()) (func(), error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, fmt.Errorf("local state directory must be private and owned by current user: %s", dir)
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("dev up already owns this checkout and route: %w", err)
	}
	// With the exclusive lock held, a leftover socket cannot have a live owner.
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, err
	}
	listener, err := net.Listen("unix", s.path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	done := make(chan struct{})
	var once sync.Once
	closeOwner := func() {
		once.Do(func() {
			listener.Close()
			close(done)
			lock.Close()
		})
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				var command [1]byte
				if _, err := io.ReadFull(conn, command[:]); err != nil {
					return
				}
				switch command[0] {
				case 'S':
					cancel()
					<-done // acknowledge only after the owner's work has stopped
				case 'P':
				default:
					return
				}
				conn.SetWriteDeadline(time.Now().Add(time.Second))
				conn.Write([]byte{'K'})
			}()
		}
	}()
	return closeOwner, nil
}

func (s State) request(command byte, timeout time.Duration) (bool, error) {
	conn, err := net.DialTimeout("unix", s.path, time.Second)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte{command}); err != nil {
		return false, err
	}
	var response [1]byte
	_, err = io.ReadFull(conn, response[:])
	// EOF during stop means the owner exited before acknowledging.
	if command == 'S' && errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if response[0] != 'K' {
		return false, fmt.Errorf("invalid local control response")
	}
	return true, nil
}

func (s State) Running() bool {
	running, _ := s.request('P', time.Second)
	return running
}

func (s State) Stop() (bool, error) {
	return s.request('S', 30*time.Second)
}
