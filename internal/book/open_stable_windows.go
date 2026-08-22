//go:build windows

package book

import (
	"os"
	"strings"
	"syscall"
)

func openStableRead(path string) (*os.File, error) {
	stablePath, err := stableWindowsPath(path)
	if err != nil {
		return nil, err
	}
	pathPtr, err := syscall.UTF16PtrFromString(stablePath)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func stableWindowsPath(path string) (string, error) {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\??\`) {
		return path, nil
	}
	fullPath, err := syscall.FullPath(path)
	if err != nil {
		return "", err
	}
	if len(fullPath) < 248 {
		return fullPath, nil
	}
	if strings.HasPrefix(fullPath, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(fullPath, `\\`), nil
	}
	return `\\?\` + fullPath, nil
}
