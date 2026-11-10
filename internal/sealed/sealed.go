// Package sealed is the web app store's encryption at rest (#197,
// docs/Decisions.md §28): the operator-held key is an age X25519 identity
// (AGE-SECRET-KEY-1…), and the store file on disk is always one whole age
// ciphertext, encrypted to that identity's recipient and to any further
// recipients the caller adds (the seam #509's recovery key uses).
//
// It is filippo.io/age's format (streaming, authenticated), not a
// hand-rolled one. It never writes a key: Seal and Open take one the caller
// holds in memory, and WriteNewKeyFile is the CLI's, on the operator's
// machine.
package sealed

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// Header is how every age file begins, so a store file that doesn't is not
// one this package wrote.
const Header = "age-encryption.org/v1\n"

// ErrWrongKey is a ciphertext the given identity doesn't decrypt: it was
// encrypted to some other key.
var ErrWrongKey = errors.New("the key doesn't decrypt the store")

// ErrNotSealed is a file that isn't age ciphertext at all, such as a
// plaintext sqlite store from before #197.
var ErrNotSealed = errors.New("not an age-encrypted file")

// GenerateKey returns a fresh age X25519 identity: 256 random bits, not a
// passphrase.
func GenerateKey() (*age.X25519Identity, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, fmt.Errorf("generate store key: %w", err)
	}
	return id, nil
}

// ParseKey parses an AGE-SECRET-KEY-1… identity, ignoring surrounding
// whitespace, and comment lines (#), as age-keygen writes them.
func ParseKey(s string) (*age.X25519Identity, error) {
	var key string
	for line := range strings.Lines(s) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key != "" {
			return nil, errors.New("store key: more than one key line")
		}
		key = line
	}
	if key == "" {
		return nil, errors.New("store key: empty; want an AGE-SECRET-KEY-1… line")
	}
	id, err := age.ParseX25519Identity(key)
	if err != nil {
		// age's message doesn't echo the key, but don't wrap it in one that
		// would either.
		return nil, fmt.Errorf("store key: not an age X25519 identity (AGE-SECRET-KEY-1…): %w", err)
	}
	return id, nil
}

// ReadKeyFile reads a key file WriteNewKeyFile (or age-keygen) wrote.
func ReadKeyFile(path string) (*age.X25519Identity, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is operator-supplied (a CLI flag)
	if err != nil {
		return nil, err
	}
	return ParseKey(string(b))
}

// WriteNewKeyFile generates a key and writes it to path, which must not
// exist yet, at mode 0600, with its recipient as a comment line, as
// age-keygen does. Refusing an existing path means a mistake can't overwrite
// the only copy of a key.
func WriteNewKeyFile(path string) (*age.X25519Identity, error) {
	id, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the path is operator-supplied (a CLI flag)
	if err != nil {
		return nil, err
	}
	_, werr := fmt.Fprintf(f, "# homelab-ops web app store key\n# public key: %s\n%s\n", id.Recipient(), id)
	if err := f.Close(); werr == nil {
		werr = err
	}
	if werr != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("write %s: %w", path, werr)
	}
	return id, nil
}

// Seal encrypts plaintext to every recipient. There must be at least one.
func Seal(plaintext []byte, recipients ...age.Recipient) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("seal: no recipients")
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipients...)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return buf.Bytes(), nil
}

// Open decrypts a whole ciphertext with id. A ciphertext for another key is
// ErrWrongKey; one that isn't age at all is ErrNotSealed; anything else
// (truncated, altered) is age's own error.
func Open(ciphertext []byte, id age.Identity) ([]byte, error) {
	if !IsSealed(ciphertext) {
		return nil, ErrNotSealed
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrWrongKey
		}
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}

// IsSealed reports whether data begins with age's header.
func IsSealed(data []byte) bool {
	return bytes.HasPrefix(data, []byte(Header))
}

// WriteFileAtomic replaces path with data whole: it writes a temp file in
// path's directory, syncs it, and renames it over path, so the disk only
// ever holds a complete file, the old one or the new one. The file is 0600.
// A symlinked path is followed (ResolveLinks): the link's target is
// replaced and the link kept, as a plain write through it would.
func WriteFileAtomic(path string, data []byte) error {
	path, err := ResolveLinks(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	// The rename is durable once the directory is synced. Best effort: not
	// every filesystem supports syncing a directory.
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: dir is the store path's own directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// maxLinks bounds ResolveLinks, as the kernel bounds a path lookup (ELOOP).
const maxLinks = 40

// ResolveLinks follows path's last element while it is a symlink and
// returns the file it ends at, which need not exist yet: a dangling link
// resolves to its target. Replacing a file by rename, as WriteFileAtomic
// does, or moving it aside must act on that file, not the link, or the link
// is replaced by a plain file and its target silently goes stale. A path
// that doesn't exist, or isn't a link, is returned as it is.
func ResolveLinks(path string) (string, error) {
	for range maxLinks {
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return path, nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return "", fmt.Errorf("%s: more than %d symlinks", path, maxLinks)
}
