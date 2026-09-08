//go:build windows

package book

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// ValidateLocalPath відхиляє мережеві та знімні носії до читання вмісту книги.
func ValidateLocalPath(path string) error {
	return validateWindowsLocalPath(path, func(root string) uint32 {
		ptr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			return windows.DRIVE_UNKNOWN
		}
		return windows.GetDriveType(ptr)
	})
}

func validateWindowsLocalPath(path string, driveType func(string) uint32) error {
	if strings.HasPrefix(strings.ReplaceAll(path, "/", `\`), `\??\`) {
		return fmt.Errorf("%w: %w", ErrNotReadable, ErrNotLocal)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	// Розширений DOS-шлях дозволений, UNC та device namespaces не підтримуються.
	absPath = strings.TrimPrefix(absPath, `\\?\`)
	volume := filepath.VolumeName(absPath)
	if len(volume) != 2 || volume[1] != ':' || !isDriveLetter(volume[0]) {
		return fmt.Errorf("%w: %w", ErrNotReadable, ErrNotLocal)
	}
	switch driveType(volume + `\`) {
	case windows.DRIVE_FIXED, windows.DRIVE_RAMDISK:
		return nil
	default:
		return fmt.Errorf("%w: %w", ErrNotReadable, ErrNotLocal)
	}
}

func isDriveLetter(letter byte) bool {
	return letter >= 'A' && letter <= 'Z' || letter >= 'a' && letter <= 'z'
}

func validateOpenedFileLocation(file *os.File) error {
	buffer := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	if n >= uint32(len(buffer)) {
		return fmt.Errorf("%w: final path is too long", ErrNotReadable)
	}
	if err := ValidateLocalPath(windows.UTF16ToString(buffer[:n])); err != nil {
		return err
	}
	// Точка монтування всередині fixed disk може вести на інший тип носія.
	volume := make([]uint16, 32768)
	if err := windows.GetVolumePathName(&buffer[0], &volume[0], uint32(len(volume))); err != nil {
		return fmt.Errorf("%w: %v", ErrNotReadable, err)
	}
	switch windows.GetDriveType(&volume[0]) {
	case windows.DRIVE_FIXED, windows.DRIVE_RAMDISK:
		return nil
	default:
		return fmt.Errorf("%w: %w", ErrNotReadable, ErrNotLocal)
	}
}
