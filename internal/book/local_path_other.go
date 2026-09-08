//go:build !windows

package book

import "os"

// ValidateLocalPath на нецільових платформах залишає файлові перевірки unit-тестам.
func ValidateLocalPath(string) error { return nil }

func validateOpenedFileLocation(*os.File) error { return nil }
