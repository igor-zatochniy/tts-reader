//go:build windows

package book

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLocalBookPathsRejectNetworkNamespaces(t *testing.T) {
	for _, path := range []string{
		`\\server\share\book.txt`, `//server/share/book.txt`,
		`\\?\UNC\server\share\book.txt`, `\\.\pipe\book`, `\??\UNC\server\share\book.txt`,
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := InspectFileContext(context.Background(), path); !errors.Is(err, ErrNotLocal) {
				t.Fatalf("мережевий шлях не відхилено: %v", err)
			}
			if file, _, err := OpenStableReadContext(context.Background(), path); !errors.Is(err, ErrNotLocal) || file != nil {
				t.Fatalf("stable read прийняв мережевий шлях: %v", err)
			}
			store := NewStoreWithProgressDir(t.TempDir())
			if _, err := store.Add(AddRequest{Path: path}); !errors.Is(err, ErrNotReadable) {
				t.Fatalf("store прийняв мережевий шлях: %v", err)
			}
		})
	}
}

func TestLocalBookPathsRejectUnsupportedDriveTypes(t *testing.T) {
	for _, kind := range []uint32{windows.DRIVE_REMOTE, windows.DRIVE_REMOVABLE, windows.DRIVE_CDROM, windows.DRIVE_UNKNOWN, windows.DRIVE_NO_ROOT_DIR} {
		err := validateWindowsLocalPath(`Z:\book.txt`, func(root string) uint32 {
			if root != `Z:\` {
				t.Fatalf("невірний корінь диска: %q", root)
			}
			return kind
		})
		if !errors.Is(err, ErrNotLocal) {
			t.Fatalf("тип диска %d не відхилено: %v", kind, err)
		}
	}
	for _, path := range []string{`C:\book.txt`, `\\?\C:\book.txt`} {
		if err := validateWindowsLocalPath(path, func(string) uint32 { return windows.DRIVE_FIXED }); err != nil {
			t.Fatalf("локальний шлях відхилено: %v", err)
		}
	}
}
