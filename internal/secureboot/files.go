package secureboot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// File is one file WriteFiles creates.
type File struct {
	Path string
	Data []byte
	// Private files (private keys) are written 0600, the rest 0644.
	Private bool
}

// WriteFiles writes files so that a failure part-way leaves every target as
// it was. Each file is first written and synced to a temporary file in its
// target's directory. Only once all of them are written are they renamed
// into place, and a rename within one directory does not run out of space
// or permission the way a write does. Writing in place could leave a mixed
// set behind: one run's PK and KEK private keys beside another run's
// certificates and .auth payloads, with the keys those payloads need gone.
//
// Without force, WriteFiles refuses, before writing anything, if any target
// already exists. That check is not atomic with the renames, so a target
// created concurrently in between is replaced; both callers are commands one
// operator runs by hand. With force, it still refuses a target that is not a
// regular file, such as a directory or a symlink, rather than replace it.
// Each target's directory must already exist.
func WriteFiles(files []File, force bool) error {
	for _, f := range files {
		st, err := os.Lstat(f.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("check %s: %w", f.Path, err)
		case !force:
			return fmt.Errorf("%s already exists (use --force to overwrite)", f.Path)
		case !st.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file; remove it first", f.Path)
		}
	}

	tmps := make([]string, 0, len(files))
	defer func() {
		for _, tmp := range tmps {
			if tmp != "" {
				_ = os.Remove(tmp)
			}
		}
	}()
	for _, f := range files {
		tmp, err := writeTemp(f)
		if err != nil {
			return err
		}
		tmps = append(tmps, tmp)
	}
	for i, f := range files {
		if err := os.Rename(tmps[i], f.Path); err != nil {
			return fmt.Errorf("move %s into place, after %d of %d files were: %w", f.Path, i, len(files), err)
		}
		tmps[i] = ""
	}
	syncDirs(files)
	return nil
}

// writeTemp writes f's data to a new temporary file beside f.Path, with f's
// mode, and syncs it. On error it removes the temporary file.
func writeTemp(f File) (path string, err error) {
	perm := fs.FileMode(0o644)
	if f.Private {
		perm = 0o600
	}
	fh, err := os.CreateTemp(filepath.Dir(f.Path), "."+filepath.Base(f.Path)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create a temporary file for %s: %w", f.Path, err)
	}
	defer func() {
		if closeErr := fh.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close %s: %w", fh.Name(), closeErr)
		}
		if err != nil {
			_ = os.Remove(fh.Name())
			path = ""
		}
	}()
	// CreateTemp makes the file 0600; certificates and payloads are public.
	if err = fh.Chmod(perm); err != nil {
		return "", fmt.Errorf("chmod %s: %w", fh.Name(), err)
	}
	if _, err = fh.Write(f.Data); err != nil {
		return "", fmt.Errorf("write %s: %w", f.Path, err)
	}
	if err = fh.Sync(); err != nil {
		return "", fmt.Errorf("sync %s: %w", f.Path, err)
	}
	return fh.Name(), nil
}

// syncDirs syncs each target's directory so the renames survive a crash.
// It is best effort: the files are already in place, and some filesystems
// do not support syncing a directory.
func syncDirs(files []File) {
	seen := map[string]bool{}
	for _, f := range files {
		dir := filepath.Dir(f.Path)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: the directory of an operator-supplied output path
			_ = d.Sync()
			_ = d.Close()
		}
	}
}
