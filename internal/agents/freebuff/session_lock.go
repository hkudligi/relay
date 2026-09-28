package freebuff

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// sessionLock is held for the complete lifetime of a Freebuff run. Freebuff
// permits only one account session, so this lock must be user-scoped rather
// than repository-scoped.
type sessionLock struct {
	file *os.File
}

func acquireSessionLock() (*sessionLock, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		root = os.TempDir()
	}
	lockDir := filepath.Join(root, "rly")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		// Sandboxed environments may expose a user config directory as
		// read-only. A per-user temp directory still provides the required
		// inter-process lock in that environment.
		lockDir = filepath.Join(os.TempDir(), fmt.Sprintf("rly-%d", os.Getuid()))
		if fallbackErr := os.MkdirAll(lockDir, 0o700); fallbackErr != nil {
			return nil, fmt.Errorf("create freebuff lock directory: %w", fallbackErr)
		}
	}
	file, err := os.OpenFile(filepath.Join(lockDir, "freebuff-session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open freebuff session lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrSessionConflict
		}
		return nil, fmt.Errorf("acquire freebuff session lock: %w", err)
	}
	return &sessionLock{file: file}, nil
}

func (l *sessionLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
