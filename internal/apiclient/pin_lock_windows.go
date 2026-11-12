//go:build windows

package apiclient

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile is the Unix lockFile's counterpart: an exclusive LockFileEx on
// path's first byte, held until the returned release.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: beside the pins file, as LookupPin
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		f.Close() //nolint:errcheck,gosec // the lock error is the one to report
		return nil, err
	}
	return func() {
		_ = windows.UnlockFileEx(h, 0, 1, 0, overlapped)
		_ = f.Close()
	}, nil
}
