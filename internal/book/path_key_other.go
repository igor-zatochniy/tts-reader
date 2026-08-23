//go:build !windows

package book

func canonicalPathKey(path string) string {
	return path
}
