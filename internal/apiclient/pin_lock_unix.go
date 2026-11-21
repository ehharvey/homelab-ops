//go:build !windows

package apiclient

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on path, creating it (0600) if needed,
// and returns its release. The lock file is left in place: removing it would
// let a waiter lock a file a newcomer no longer sees.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: beside the pins file, as LookupPin
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd()) //nolint:gosec // G115: a file descriptor fits in an int
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		f.Close() //nolint:errcheck,gosec // the lock error is the one to report
		return nil, err
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
