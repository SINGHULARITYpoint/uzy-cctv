//go:build unix

package devices

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type osLockFile struct {
	fd int
}

func openOSLockFile(path string) (*osLockFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return &osLockFile{fd: fd}, nil
}

func (f *osLockFile) tryLock() (bool, error) {
	err := syscall.Flock(f.fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func (f *osLockFile) unlockAndClose() {
	if f == nil || f.fd < 0 {
		return
	}
	_ = syscall.Flock(f.fd, syscall.LOCK_UN)
	_ = syscall.Close(f.fd)
	f.fd = -1
}

func (f *osLockFile) closeOnly() {
	if f == nil || f.fd < 0 {
		return
	}
	_ = syscall.Close(f.fd)
	f.fd = -1
}
