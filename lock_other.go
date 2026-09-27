//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package stellarjay

import (
	"errors"
	"os"
	"path/filepath"
)

type storeLock struct {
	file *os.File
	path string
}

func acquireStoreLock(dir string) (*storeLock, error) {
	path := filepath.Join(dir, ".writer.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, appErr(ErrConflict, "store is already open by another process or Store instance")
	}
	if err != nil {
		return nil, err
	}
	return &storeLock{file: file, path: path}, nil
}

func (l *storeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	closeErr := l.file.Close()
	removeErr := os.Remove(l.path)
	l.file = nil
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
