//go:build !windows

package main

import "os"

func replaceProgressFile(tmpName string, targetName string) error {
	return os.Rename(tmpName, targetName)
}
