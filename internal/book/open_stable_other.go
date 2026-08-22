//go:build !windows

package book

import "os"

func openStableRead(path string) (*os.File, error) {
	return os.Open(path)
}
