//go:build !windows

package book

func canonicalPathKey(path string) string {
	return path
}

func legacyPathKey(path string) string {
	return path
}
