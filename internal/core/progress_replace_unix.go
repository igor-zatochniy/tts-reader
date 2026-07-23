//go:build !windows

package core

import "os"

func replaceProgressFile(tmpName string, targetName string) error {
	return os.Rename(tmpName, targetName)
}
