package progress

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var ErrInUse = errors.New("progress file is already in use")

var activeLeases = struct {
	sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

type Lease struct {
	key      string
	platform platformLease
	once     sync.Once
	err      error
}

type platformLease interface {
	Close() error
}

func AcquireLease(progressPath string) (*Lease, error) {
	if strings.TrimSpace(progressPath) == "" {
		return nil, fmt.Errorf("progress path is empty")
	}

	lockPath, err := filepath.Abs(progressPath + ".lock")
	if err != nil {
		return nil, fmt.Errorf("resolve progress lock path: %w", err)
	}
	lockPath = filepath.Clean(lockPath)
	lockDir := filepath.Dir(lockPath)
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return nil, fmt.Errorf("create progress lock directory: %w", err)
	}
	if resolvedDir, err := filepath.EvalSymlinks(lockDir); err == nil {
		lockPath = filepath.Join(resolvedDir, filepath.Base(lockPath))
	}
	key := lockPath
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}

	activeLeases.Lock()
	if _, exists := activeLeases.paths[key]; exists {
		activeLeases.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrInUse, progressPath)
	}
	activeLeases.paths[key] = struct{}{}
	activeLeases.Unlock()

	releaseKey := true
	defer func() {
		if releaseKey {
			activeLeases.Lock()
			delete(activeLeases.paths, key)
			activeLeases.Unlock()
		}
	}()

	platform, err := acquirePlatformLease(lockPath)
	if err != nil {
		if errors.Is(err, ErrInUse) {
			return nil, fmt.Errorf("%w: %s", ErrInUse, progressPath)
		}
		return nil, fmt.Errorf("acquire progress lock: %w", err)
	}

	releaseKey = false
	return &Lease{key: key, platform: platform}, nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if err := l.platform.Close(); err != nil {
			l.err = fmt.Errorf("release progress lock: %w", err)
		}
		activeLeases.Lock()
		delete(activeLeases.paths, l.key)
		activeLeases.Unlock()
	})
	return l.err
}
