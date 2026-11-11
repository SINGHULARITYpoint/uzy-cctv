//go:build windows

package devices

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type osLockFile struct {
	handle windows.Handle
}

func openOSLockFile(path string) (*osLockFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode lock path: %w", err)
	}
	handle, err := windows.CreateFile(
		p16,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return &osLockFile{handle: handle}, nil
}

func (f *osLockFile) tryLock() (bool, error) {
	var ol windows.Overlapped
	err := windows.LockFileEx(
		f.handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&ol,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

func (f *osLockFile) unlockAndClose() {
	if f == nil || f.handle == 0 || f.handle == windows.InvalidHandle {
		return
	}
	var ol windows.Overlapped
	_ = windows.UnlockFileEx(f.handle, 0, 1, 0, &ol)
	_ = windows.CloseHandle(f.handle)
	f.handle = windows.InvalidHandle
}

func (f *osLockFile) closeOnly() {
	if f == nil || f.handle == 0 || f.handle == windows.InvalidHandle {
		return
	}
	_ = windows.CloseHandle(f.handle)
	f.handle = windows.InvalidHandle
}
