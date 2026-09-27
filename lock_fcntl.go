//go:build aix || (solaris && !illumos)

package stellarjay

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type storeLock struct {
	file *os.File
	path string
}

var fcntlProcessLocks = struct {
	sync.Mutex
	held map[string]bool
}{held: make(map[string]bool)}

func acquireStoreLock(dir string) (*storeLock, error) {
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(canonicalDir, ".writer.lock")
	fcntlProcessLocks.Lock()
	if fcntlProcessLocks.held[path] {
		fcntlProcessLocks.Unlock()
		return nil, appErr(ErrConflict, "store is already open by another process or Store instance")
	}
	fcntlProcessLocks.held[path] = true
	fcntlProcessLocks.Unlock()
	reserved := true
	defer func() {
		if reserved {
			fcntlProcessLocks.Lock()
			delete(fcntlProcessLocks.held, path)
			fcntlProcessLocks.Unlock()
		}
	}()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
		file.Close()
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EAGAIN) {
			return nil, appErr(ErrConflict, "store is already open by another process or Store instance")
		}
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	reserved = false
	return &storeLock{file: file, path: path}, nil
}

func (l *storeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	lock := syscall.Flock_t{Type: syscall.F_UNLCK}
	unlockErr := syscall.FcntlFlock(l.file.Fd(), syscall.F_SETLK, &lock)
	closeErr := l.file.Close()
	l.file = nil
	fcntlProcessLocks.Lock()
	delete(fcntlProcessLocks.held, l.path)
	fcntlProcessLocks.Unlock()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
