//go:build !windows

package progress

import "os"

func replaceProgressFile(tmpName string, targetName string) error {
	return os.Rename(tmpName, targetName)
}
