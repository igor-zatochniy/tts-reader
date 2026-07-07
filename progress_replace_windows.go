//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

const (
	movefileReplaceExisting = 0x1
	movefileWriteThrough    = 0x8
)

var procMoveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceProgressFile(tmpName string, targetName string) error {
	tmpPtr, err := syscall.UTF16PtrFromString(tmpName)
	if err != nil {
		return err
	}
	targetPtr, err := syscall.UTF16PtrFromString(targetName)
	if err != nil {
		return err
	}

	result, _, callErr := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(tmpPtr)),
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)
	if result != 0 {
		return nil
	}
	if callErr != syscall.Errno(0) {
		return callErr
	}
	return syscall.EINVAL
}
