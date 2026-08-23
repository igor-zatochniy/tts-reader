//go:build windows

package book

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func canonicalPathKey(path string) string {
	caseSensitive, err := pathUsesCaseSensitiveNames(path)
	if err != nil {
		// Точний ключ безпечніший за помилкове об'єднання різних файлів.
		return path
	}
	return windowsPathKey(path, caseSensitive)
}

func windowsPathKey(path string, caseSensitive bool) string {
	if caseSensitive {
		return path
	}
	return strings.ToLower(path)
}

func pathUsesCaseSensitiveNames(path string) (bool, error) {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		caseSensitive, err := directoryIsCaseSensitive(dir)
		if err != nil {
			return false, err
		}
		if caseSensitive {
			return true, nil
		}
		if parent := filepath.Dir(dir); parent == dir {
			return false, nil
		}
	}
}

func directoryIsCaseSensitive(path string) (bool, error) {
	stablePath, err := stableWindowsPath(path)
	if err != nil {
		return false, err
	}
	pathPtr, err := windows.UTF16PtrFromString(stablePath)
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(handle)

	var flags uint32
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&flags)),
		uint32(unsafe.Sizeof(flags)),
	); err != nil {
		return false, err
	}
	return flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0, nil
}
