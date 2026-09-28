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
	lockDirs := []string{
		filepath.Join(root, "rly"),
		filepath.Join(os.TempDir(), fmt.Sprintf("rly-%d", os.Getuid())),
	}
	var lastErr error
	for _, lockDir := range lockDirs {
		if err := os.MkdirAll(lockDir, 0o700); err != nil {
			lastErr = fmt.Errorf("create freebuff lock directory: %w", err)
			continue
		}
		file, err := os.OpenFile(filepath.Join(lockDir, "freebuff-session.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			lastErr = fmt.Errorf("open freebuff session lock: %w", err)
			continue
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
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("open freebuff session lock: no lock directories available")
}

func (l *sessionLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
