package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The store's whole-database bytes, for encryption at rest (#197,
// docs/Decisions.md §28). The web app works on an in-memory store and
// writes these bytes back, sealed, after each change, so plaintext never
// reaches the disk: sqlite's serialize/deserialize API copies the database
// to and from memory directly, where a VACUUM INTO round-trip would need a
// plaintext file.

// serializer is the modernc.org/sqlite driver connection's
// sqlite3_serialize/sqlite3_deserialize.
type serializer interface {
	Serialize() ([]byte, error)
	Deserialize(buf []byte) error
}

// Serialize returns the whole database as sqlite's file format, the bytes a
// file-backed copy of it would hold.
func (s *Store) Serialize(ctx context.Context) ([]byte, error) {
	var out []byte
	err := withSerializer(ctx, s.db, func(c serializer) error {
		b, err := c.Serialize()
		out = b
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("serialize store: %w", err)
	}
	return out, nil
}

// OpenSerialized opens an in-memory store holding a copy of data, which
// Serialize returned, and migrates it as Open does a file.
func OpenSerialized(ctx context.Context, data []byte) (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open in-memory store: %w", err)
	}
	// One connection, as in Open; here it also matters for correctness:
	// every connection to ":memory:" is its own database, so the one that
	// was deserialized into must be the only one.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db}
	if err := s.Restore(ctx, data); err != nil {
		db.Close() //nolint:errcheck,gosec // best-effort cleanup on a failed open
		return nil, err
	}
	return s, nil
}

// Restore replaces the whole database with a copy of data, which Serialize
// returned, and migrates it as Open does a file. internal/vault uses it to
// undo a change whose write to disk failed. The store must have one
// connection (OpenSerialized's, or Open's ":memory:"), so the one restored
// into is the only one.
func (s *Store) Restore(ctx context.Context, data []byte) error {
	if err := withSerializer(ctx, s.db, func(c serializer) error { return c.Deserialize(data) }); err != nil {
		return fmt.Errorf("load store: %w", err)
	}
	return migrate(ctx, s.db)
}

func withSerializer(ctx context.Context, db *sql.DB, f func(serializer) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // returns the connection to the pool
	return conn.Raw(func(dc any) error {
		c, ok := dc.(serializer)
		if !ok {
			return errors.New("the sqlite driver can't serialize")
		}
		return f(c)
	})
}
