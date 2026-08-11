//go:build !windows && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package progress

type unsupportedLease struct{}

func acquirePlatformLease(string) (platformLease, error) {
	return unsupportedLease{}, nil
}

func (unsupportedLease) Close() error {
	return nil
}
