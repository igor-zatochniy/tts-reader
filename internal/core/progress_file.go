package core

import (
	"os"
	"path/filepath"
)

func writeFileReplace(path string, data []byte, perm os.FileMode) error {
	return writeFileReplaceWith(path, data, perm, replaceProgressFile)
}

func writeFileReplaceWith(path string, data []byte, perm os.FileMode, replace func(string, string) error) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}

	tmpName := tmp.Name()
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := replace(tmpName, path); err != nil {
		return err
	}

	keepTemp = false
	return nil
}

func progressPercent(pos int64, total int64) float64 {
	if total == 0 {
		return 100
	}
	return (float64(pos) / float64(total)) * 100
}

func ProgressPercent(pos int64, total int64) float64 {
	return progressPercent(pos, total)
}
