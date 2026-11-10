// Package vault is the web app store's lock (#197, docs/Decisions.md §28):
// after every start the store is locked, and nothing can read or write it
// until the operator's key, an age X25519 identity, is supplied.
//
// The working copy is an in-memory sqlite store (internal/store). With a
// store file (STORE_PATH), the file is one whole age ciphertext
// (internal/sealed), rewritten after each change via a temp file and a
// rename; it is never plaintext. Without one (":memory:"), every start
// begins empty, and is still locked until init.
//
// The verbs are separate so that a mistake can't destroy state: Init (no
// store yet), Unlock (a store file exists), Rotate (unlocked, old key and
// new) and Reset (locked, the key lost: the old file is moved aside, never
// deleted).
//
// The key is held in memory only, and never written. How it arrives is the
// caller's (the API today; a TPM unseal and a recovery key later, #509):
// Vault takes an identity, whatever supplied it. The recipients the file is
// sealed to are the held identity's plus Extra, the seam for #509's
// recovery key.
package vault

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/ehharvey/homelab-ops/internal/config"
	"github.com/ehharvey/homelab-ops/internal/nodeprovision"
	"github.com/ehharvey/homelab-ops/internal/sealed"
	"github.com/ehharvey/homelab-ops/internal/store"
)

// State is the lock state, as GET /store/state reports it. The values are a
// stable, machine-readable contract (#508's UI shows them, §32).
type State string

const (
	// NoStore is no store yet: no store file, or an in-memory store after a
	// start. Only Init is accepted.
	NoStore State = "no_store"
	// Locked is a store file whose key hasn't been supplied. Only Unlock
	// and Reset are accepted.
	Locked State = "locked"
	// Unlocked is the key held and the store open.
	Unlocked State = "unlocked"
)

// The verbs' refusals, for the API to map to status codes.
var (
	// ErrLocked is any store read or write while the vault isn't unlocked.
	ErrLocked = errors.New("store is locked")
	// ErrState is a verb the current state doesn't accept.
	ErrState = errors.New("not accepted in this lock state")
	// ErrWrongKey is a key that doesn't decrypt the store, or (Rotate) isn't
	// the one held.
	ErrWrongKey = sealed.ErrWrongKey
)

// Vault holds the store and its lock. It implements internal/server.Store and
// internal/nodeprovision.CredentialStore over the open store, refusing every
// call with ErrLocked while it isn't unlocked.
type Vault struct {
	// path is the store file, with a symlink resolved to its target, or ""
	// for an in-memory store.
	path string
	// Extra recipients every write is also sealed to. Unused today; #509's
	// recovery key is one. Set before the first verb.
	Extra []age.Recipient

	// mu guards the lock state (st and id) and saved. Reads hold it for
	// reading; the verbs and every change hold it for writing, from the
	// change through its write to the file, so no read sees a change before
	// the file has it (or after a failed write undid it), and the file
	// always ends as the latest change left the store.
	mu sync.RWMutex
	st *store.Store
	id *age.X25519Identity
	// saved is the store's bytes as the file last took them, to undo a
	// change whose write failed.
	saved []byte
	// now is time.Now, swapped by tests for reset's moved-aside name.
	now func() time.Time
	// writeFile is sealed.WriteFileAtomic, swapped by tests to fail a write.
	writeFile func(path string, data []byte) error
}

// New returns a locked vault over the store file at path. path ":memory:"
// (or "") is an in-memory store: it starts in NoStore at every start. A
// symlinked path is the link's target, which need not exist yet. A file
// that exists but isn't age ciphertext is refused: there is no migration
// from a plaintext store (operator, 2026-11-05).
func New(path string) (*Vault, error) {
	if path == ":memory:" {
		path = ""
	}
	v := &Vault{now: time.Now, writeFile: sealed.WriteFileAtomic}
	if path == "" {
		return v, nil
	}
	resolved, err := sealed.ResolveLinks(path)
	if err != nil {
		return nil, fmt.Errorf("STORE_PATH: %w", err)
	}
	v.path = resolved
	f, err := os.Open(resolved) //nolint:gosec // G304: STORE_PATH is operator-supplied deployment config
	if errors.Is(err, fs.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, fmt.Errorf("STORE_PATH: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	head := make([]byte, len(sealed.Header))
	n, err := io.ReadFull(f, head)
	switch {
	case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
		return nil, fmt.Errorf("STORE_PATH %s: %w", path, err)
	case n == 0:
		return nil, fmt.Errorf("STORE_PATH %s is an empty file, not a store: remove it, then init", path)
	case !sealed.IsSealed(head[:n]):
		return nil, fmt.Errorf("STORE_PATH %s: %w; a plaintext store from before #197 isn't migrated: move it aside, then init", path, sealed.ErrNotSealed)
	}
	return v, nil
}

// Persistent reports whether the store has a file (STORE_PATH set).
func (v *Vault) Persistent() bool { return v.path != "" }

// State is the current lock state.
func (v *Vault) State() State {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.stateLocked()
}

// stateLocked is State with mu held.
func (v *Vault) stateLocked() State {
	switch {
	case v.st != nil:
		return Unlocked
	case v.path != "" && fileExists(v.path):
		return Locked
	default:
		return NoStore
	}
}

// Init starts an empty store under id. Accepted only in NoStore.
func (v *Vault) Init(ctx context.Context, id *age.X25519Identity) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s := v.stateLocked(); s != NoStore {
		return fmt.Errorf("init: the store is %s: %w", s, ErrState)
	}
	return v.startEmpty(ctx, id)
}

// startEmpty opens an empty store under id and, with a file, writes it.
// mu must be held for writing.
func (v *Vault) startEmpty(ctx context.Context, id *age.X25519Identity) error {
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		return err
	}
	if err := v.write(ctx, st, id); err != nil {
		st.Close() //nolint:errcheck,gosec // best-effort cleanup of a store that never opened
		return err
	}
	v.st, v.id = st, id
	return nil
}

// Unlock decrypts the store file with id and opens it. A wrong key is
// ErrWrongKey, and nothing is written. Accepted only in Locked.
func (v *Vault) Unlock(ctx context.Context, id *age.X25519Identity) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s := v.stateLocked(); s != Locked {
		return fmt.Errorf("unlock: the store is %s: %w", s, ErrState)
	}
	ciphertext, err := os.ReadFile(v.path)
	if err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	plain, err := sealed.Open(ciphertext, id)
	if err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	st, err := store.OpenSerialized(ctx, plain)
	if err != nil {
		return fmt.Errorf("unlock: %w", err)
	}
	v.st, v.id, v.saved = st, id, plain
	return nil
}

// Rotate re-encrypts the store from old, which must be the key held, to
// next. Accepted only in Unlocked. State is kept, and old no longer unlocks.
func (v *Vault) Rotate(ctx context.Context, old, next *age.X25519Identity) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s := v.stateLocked(); s != Unlocked {
		return fmt.Errorf("rotate: the store is %s: %w", s, ErrState)
	}
	// Constant time, so a refusal's timing says nothing about the held key.
	if subtle.ConstantTimeCompare([]byte(old.String()), []byte(v.id.String())) != 1 {
		return fmt.Errorf("rotate: the old key isn't the store's: %w", ErrWrongKey)
	}
	if err := v.write(ctx, v.st, next); err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	v.id = next
	return nil
}

// Reset is for a lost key: it moves the store file aside, to the returned
// path, and starts an empty store under next. Accepted only in Locked, so
// it can't discard a store whose key is held; the caller confirms it.
// Once the file is moved, the rest outlasts ctx's cancellation, and a
// failure moves the old file back: a reset either finishes or leaves the
// store as it was, never moved aside with no store in its place.
func (v *Vault) Reset(ctx context.Context, next *age.X25519Identity) (movedTo string, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if s := v.stateLocked(); s != Locked {
		return "", fmt.Errorf("reset: the store is %s: %w", s, ErrState)
	}
	movedTo = fmt.Sprintf("%s.reset-%s", v.path, v.now().UTC().Format("20060102T150405Z"))
	if fileExists(movedTo) {
		return "", fmt.Errorf("reset: %s already exists; try again in a second", movedTo)
	}
	if err := os.Rename(v.path, movedTo); err != nil {
		return "", fmt.Errorf("reset: move the old store aside: %w", err)
	}
	if err := v.startEmpty(context.WithoutCancel(ctx), next); err != nil {
		if rerr := os.Rename(movedTo, v.path); rerr != nil {
			return movedTo, fmt.Errorf("reset: %w; moving the old store back also failed, so it is at %s: %w", err, movedTo, rerr)
		}
		return "", fmt.Errorf("reset: %w (the old store is back in place)", err)
	}
	return movedTo, nil
}

// write seals st to id's recipient and Extra and replaces the file with it,
// then records the bytes as saved; a no-op for an in-memory store. mu must
// be held for writing.
func (v *Vault) write(ctx context.Context, st *store.Store, id *age.X25519Identity) error {
	if v.path == "" {
		return nil
	}
	plain, err := st.Serialize(ctx)
	if err != nil {
		return err
	}
	ciphertext, err := sealed.Seal(plain, append([]age.Recipient{id.Recipient()}, v.Extra...)...)
	if err != nil {
		return err
	}
	if err := v.writeFile(v.path, ciphertext); err != nil {
		return fmt.Errorf("write store: %w", err)
	}
	v.saved = plain
	return nil
}

// Close closes the open store, if any.
func (v *Vault) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.st == nil {
		return nil
	}
	err := v.st.Close()
	v.st = nil
	return err
}

// fileExists follows a symlink, so a dangling one is no file.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// with runs f on the open store, with mu held for reading, or refuses with
// ErrLocked.
func (v *Vault) with(f func(*store.Store) error) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.st == nil {
		return ErrLocked
	}
	return f(v.st)
}

// change runs f on the open store, then writes the store file, or refuses
// with ErrLocked. See update.
func (v *Vault) change(ctx context.Context, f func(*store.Store) error) error {
	return v.update(ctx, func(st *store.Store) (bool, error) { return true, f(st) })
}

// update runs f on the open store and, if f reports a change, writes the
// store file; it refuses with ErrLocked. mu is held for writing throughout,
// so no read sees f's change before the file has it. A failed write is
// returned, and f's change undone in memory, so a later read never sees a
// change the file didn't take: a credential minted then lost would
// otherwise be served (and flashed) until a restart forgot it. The write
// and the undo outlast ctx's cancellation, so a caller that goes away can't
// leave a change applied but unwritten.
func (v *Vault) update(ctx context.Context, f func(*store.Store) (changed bool, err error)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.st == nil {
		return ErrLocked
	}
	changed, err := f(v.st)
	if err != nil || !changed {
		return err
	}
	wctx := context.WithoutCancel(ctx)
	err = v.write(wctx, v.st, v.id)
	if err == nil {
		return nil
	}
	if rerr := v.st.Restore(wctx, v.saved); rerr != nil {
		return fmt.Errorf("%w; undoing the change in memory also failed, so reads show it until a restart: %w", err, rerr)
	}
	return fmt.Errorf("%w (the change was undone)", err)
}

// The store's methods, over the open store (internal/server.Store,
// internal/nodeprovision.CredentialStore).

// Replace is store.Store.Replace, then a write, unless the synced snapshot
// (store.Store.SnapshotDigest: the fleet and the commit) is unchanged. A
// poll of an unchanged repo moves only the sync time, which isn't worth a
// rewrite of the whole store: the new time is kept in memory, and reaches
// the file with the next change. After a restart, LastSync's time is
// therefore that of the last sync written: one that changed the snapshot,
// or the last before another change.
func (v *Vault) Replace(ctx context.Context, cfg config.Config, commit string, now time.Time) error {
	return v.update(ctx, func(st *store.Store) (bool, error) {
		before, berr := st.SnapshotDigest(ctx)
		if err := st.Replace(ctx, cfg, commit, now); err != nil {
			return false, err
		}
		after, aerr := st.SnapshotDigest(ctx)
		return berr != nil || aerr != nil || before != after, nil
	})
}

// SetInstanceCredential is store.Store.SetInstanceCredential, then a write.
func (v *Vault) SetInstanceCredential(ctx context.Context, name string, cred nodeprovision.Credential) error {
	return v.change(ctx, func(st *store.Store) error { return st.SetInstanceCredential(ctx, name, cred) })
}

// BatchCredentials runs f with the open store as its credential store, then
// writes the store file once if f set any credential, rather than once per
// credential: internal/server mints its tunnel peers' credentials this way,
// so an unlock or a sync that mints N writes the file once. mu is held for
// writing throughout, so f must not call the vault, and no read sees a
// credential f set before the file has it. If the write fails, every
// credential f set is undone and the error returned. A locked vault is
// ErrLocked, and f isn't run.
func (v *Vault) BatchCredentials(ctx context.Context, f func(nodeprovision.CredentialStore)) error {
	return v.update(ctx, func(st *store.Store) (bool, error) {
		r := &credentialRecorder{Store: st}
		f(r)
		return r.set, nil
	})
}

// credentialRecorder is the open store as a nodeprovision.CredentialStore
// that records whether a credential was set.
type credentialRecorder struct {
	*store.Store
	set bool
}

func (r *credentialRecorder) SetInstanceCredential(ctx context.Context, name string, cred nodeprovision.Credential) error {
	r.set = true
	return r.Store.SetInstanceCredential(ctx, name, cred)
}

// LastSync is store.Store.LastSync.
func (v *Vault) LastSync(ctx context.Context) (commit string, syncedAt time.Time, ok bool, err error) {
	err = v.with(func(st *store.Store) error {
		commit, syncedAt, ok, err = st.LastSync(ctx)
		return err
	})
	return commit, syncedAt, ok, err
}

// Networks is store.Store.Networks.
func (v *Vault) Networks(ctx context.Context) (nets []config.Network, err error) {
	err = v.with(func(st *store.Store) error { nets, err = st.Networks(ctx); return err })
	return nets, err
}

// Instances is store.Store.Instances.
func (v *Vault) Instances(ctx context.Context) (insts []config.Instance, err error) {
	err = v.with(func(st *store.Store) error { insts, err = st.Instances(ctx); return err })
	return insts, err
}

// InstanceTunnelIPs is store.Store.InstanceTunnelIPs.
func (v *Vault) InstanceTunnelIPs(ctx context.Context) (insts []config.Instance, err error) {
	err = v.with(func(st *store.Store) error { insts, err = st.InstanceTunnelIPs(ctx); return err })
	return insts, err
}

// Apps is store.Store.Apps.
func (v *Vault) Apps(ctx context.Context) (apps []config.App, err error) {
	err = v.with(func(st *store.Store) error { apps, err = st.Apps(ctx); return err })
	return apps, err
}

// Network is store.Store.Network.
func (v *Vault) Network(ctx context.Context, name string) (n config.Network, ok bool, err error) {
	err = v.with(func(st *store.Store) error { n, ok, err = st.Network(ctx, name); return err })
	return n, ok, err
}

// Instance is store.Store.Instance.
func (v *Vault) Instance(ctx context.Context, name string) (i config.Instance, ok bool, err error) {
	err = v.with(func(st *store.Store) error { i, ok, err = st.Instance(ctx, name); return err })
	return i, ok, err
}

// InstanceCredential is store.Store.InstanceCredential.
func (v *Vault) InstanceCredential(ctx context.Context, name string) (c nodeprovision.Credential, ok bool, err error) {
	err = v.with(func(st *store.Store) error { c, ok, err = st.InstanceCredential(ctx, name); return err })
	return c, ok, err
}
