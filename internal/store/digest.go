package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
)

// snapshotQueries read what Replace writes, but its sync time: every row of
// the three fleet tables, in a fixed order, and the synced commit. The
// credentials aren't Replace's, so they aren't here.
var snapshotQueries = []string{
	"SELECT * FROM networks ORDER BY name",
	"SELECT * FROM instances ORDER BY name",
	"SELECT * FROM apps ORDER BY name",
	"SELECT commit_sha FROM sync_state",
}

// SnapshotDigest hashes the synced snapshot: the fleet Replace stored and
// its commit, but not the sync time. internal/vault compares it before and
// after a Replace, so a sync that moved only the time isn't written to the
// encrypted store file (#197).
func (s *Store) SnapshotDigest(ctx context.Context) ([sha256.Size]byte, error) {
	h := sha256.New()
	for _, q := range snapshotQueries {
		if err := digestRows(ctx, s, q, h); err != nil {
			return [sha256.Size]byte{}, fmt.Errorf("digest snapshot: %w", err)
		}
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// digestRows writes each row of q's result to w, every value quoted with
// its type and each row and query terminated, so no two results write the
// same bytes.
func digestRows(ctx context.Context, s *Store, q string, w io.Writer) error {
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck // read-only; rows.Err reports a failed read
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for _, v := range vals {
			if _, err := fmt.Fprintf(w, "%T %q\x1f", v, fmt.Sprint(v)); err != nil {
				return err
			}
		}
		if _, err := w.Write([]byte{0x1e}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = w.Write([]byte{0x1d})
	return err
}
