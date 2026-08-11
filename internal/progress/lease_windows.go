//go:build windows

package progress

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

const (
	windowsErrorSharingViolation syscall.Errno = 32
	windowsErrorLockViolation    syscall.Errno = 33
)

type windowsLease struct {
	handle syscall.Handle
}

func acquirePlatformLease(path string) (platformLease, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windowsErrorSharingViolation) || errors.Is(err, windowsErrorLockViolation) {
			return nil, ErrInUse
		}
		return nil, err
	}
	return &windowsLease{handle: handle}, nil
}

func (l *windowsLease) Close() error {
	return syscall.CloseHandle(l.handle)
}
