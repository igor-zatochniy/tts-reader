//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package progress

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type unixLease struct {
	file *os.File
}

func acquirePlatformLease(path string) (platformLease, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrInUse
		}
		return nil, err
	}
	return &unixLease{file: file}, nil
}

func (l *unixLease) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return errors.Join(unlockErr, l.file.Close())
}
