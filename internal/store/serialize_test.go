package store

import (
	"bytes"
	"reflect"
	"testing"
	"time"
)

// A serialized store reopens, in memory, with everything it held, and it
// is sqlite's file format: what the vault seals.
func TestSerializeRoundTrips(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := s.Replace(ctx, sampleConfig(), "abc123", now); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	data, err := s.Serialize(ctx)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		t.Fatalf("Serialize didn't return sqlite's file format: %q", data[:min(len(data), 16)])
	}

	got, err := OpenSerialized(ctx, data)
	if err != nil {
		t.Fatalf("OpenSerialized: %v", err)
	}
	defer got.Close() //nolint:errcheck // test cleanup

	commit, at, ok, err := got.LastSync(ctx)
	if err != nil || !ok || commit != "abc123" || !at.Equal(now) {
		t.Errorf("LastSync = %q %v %v %v, want abc123 at %v", commit, at, ok, err, now)
	}
	want, err := s.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	have, err := got.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("Instances after the round trip = %+v, want %+v", have, want)
	}

	// The copy is its own database: a write to it doesn't reach the original.
	if err := got.Replace(ctx, sampleConfig(), "def456", now); err != nil {
		t.Fatalf("Replace on the copy: %v", err)
	}
	if commit, _, _, _ := s.LastSync(ctx); commit != "abc123" {
		t.Errorf("the original's commit became %q", commit)
	}
}

// Restore puts an open store back as it was serialized: a later change is
// gone, and the store is still usable.
func TestRestoreUndoesALaterChange(t *testing.T) {
	s, ctx := openTestStore(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := s.Replace(ctx, sampleConfig(), "abc123", now); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	saved, err := s.Serialize(ctx)
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	if err := s.Replace(ctx, sampleConfig(), "def456", now); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if err := s.Restore(ctx, saved); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if commit, _, _, err := s.LastSync(ctx); err != nil || commit != "abc123" {
		t.Errorf("LastSync after Restore = %q %v, want abc123", commit, err)
	}
	if err := s.Replace(ctx, sampleConfig(), "fed789", now); err != nil {
		t.Errorf("Replace after Restore: %v", err)
	}
}

func TestOpenSerializedRefusesJunk(t *testing.T) {
	_, ctx := openTestStore(t)
	if st, err := OpenSerialized(ctx, []byte("not a database")); err == nil {
		st.Close() //nolint:errcheck,gosec // test cleanup
		t.Error("OpenSerialized(junk) = nil error, want one")
	}
}
