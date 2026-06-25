package storage

import (
	"path/filepath"
	"regexp"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestArtifactInsertGetRoundTrip — InsertArtifact fills id (artifact_<8hex>) and
// createdAt from the clock when zero, and GetArtifact returns the content verbatim.
func TestArtifactInsertGetRoundTrip(t *testing.T) {
	idRe := regexp.MustCompile(`^artifact_[0-9a-f]{8}$`)
	s := openTest(t, 4242)
	rec, err := s.InsertArtifact(domain.ArtifactRecord{
		SessionID: "ses_a", Content: `{"ok":true}`, TotalChars: 11, TotalBytes: 11,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !idRe.MatchString(rec.ID) {
		t.Fatalf("id should be artifact_<8hex>, got %q", rec.ID)
	}
	if rec.CreatedAt != 4242 {
		t.Fatalf("createdAt should default to the clock, got %d", rec.CreatedAt)
	}
	got, ok := s.GetArtifact(rec.ID)
	if !ok {
		t.Fatal("just-inserted artifact must be retrievable")
	}
	if got != `{"ok":true}` {
		t.Fatalf("content round-trip wrong: %q", got)
	}
}

// TestArtifactExplicitIDAndTimestampPreserved — a caller-supplied id + createdAt are
// kept verbatim (set() mirrors under the SAME id the hot cache used, so the DB row
// must not be re-keyed).
func TestArtifactExplicitIDAndTimestampPreserved(t *testing.T) {
	s := openTest(t, 1)
	rec, err := s.InsertArtifact(domain.ArtifactRecord{
		ID: "artifact_deadbeef", SessionID: "ses_x", Content: "payload",
		TotalChars: 7, TotalBytes: 7, CreatedAt: 999,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rec.ID != "artifact_deadbeef" || rec.CreatedAt != 999 {
		t.Fatalf("explicit id/createdAt must be preserved, got %q / %d", rec.ID, rec.CreatedAt)
	}
	if got, ok := s.GetArtifact("artifact_deadbeef"); !ok || got != "payload" {
		t.Fatalf("explicit-id lookup failed: %q %v", got, ok)
	}
}

// TestArtifactGetMissReturnsFalse — an unknown id is a clean miss, never an error.
func TestArtifactGetMissReturnsFalse(t *testing.T) {
	s := openTest(t, 1)
	if got, ok := s.GetArtifact("artifact_00000000"); ok || got != "" {
		t.Fatalf("unknown id must miss cleanly, got %q %v", got, ok)
	}
}

// TestArtifactMultibyteSizesPreserved — TotalChars (runes) and TotalBytes (bytes)
// are stored independently; a multibyte payload keeps bytes > chars.
func TestArtifactMultibyteSizesPreserved(t *testing.T) {
	s := openTest(t, 1)
	content := "世界世界" // 4 runes, 12 bytes
	rec, err := s.InsertArtifact(domain.ArtifactRecord{
		SessionID: "ses_m", Content: content, TotalChars: 4, TotalBytes: 12,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if rec.TotalChars != 4 || rec.TotalBytes != 12 {
		t.Fatalf("multibyte sizes mangled: %d chars / %d bytes", rec.TotalChars, rec.TotalBytes)
	}
	if got, _ := s.GetArtifact(rec.ID); got != content {
		t.Fatalf("multibyte content round-trip wrong: %q", got)
	}
}

// TestArtifactSurvivesReopen is the core durability guarantee: an artifact written in
// one session must resolve via GetArtifact after the store is Closed and re-Opened
// against the SAME file — the in-memory hot cache is gone, the DB row is the
// fallback that keeps a rehydrated truncation stub readable across a restart.
func TestArtifactSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := openFile(t, path, 1)
	rec, err := first.InsertArtifact(domain.ArtifactRecord{
		SessionID: "ses_first", Content: "durable payload", TotalChars: 15, TotalBytes: 15,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A fresh process: new store, new (empty) hot cache, same file.
	second := openFile(t, path, 2)
	defer second.Close()
	got, ok := second.GetArtifact(rec.ID)
	if !ok {
		t.Fatal("artifact must survive Close()+Open() (durable fallback)")
	}
	if got != "durable payload" {
		t.Fatalf("content after reopen wrong: %q", got)
	}
}

// TestArtifactRetentionSweep — the artifacts table ages out under the same
// age+count policy as the conversation it references (a stub must not outlive its
// payload). An ancient row past the window is swept; a recent one is kept; the count
// floor retains the newest even when all are old.
func TestArtifactRetentionSweep(t *testing.T) {
	ret := baseRet()
	ret.ArtifactsKeepRows = 1
	s := gcStore(t, ret)

	old, _ := s.InsertArtifact(domain.ArtifactRecord{
		SessionID: "ses", Content: "old", TotalChars: 3, TotalBytes: 3, CreatedAt: gcOLD,
	})
	recent, _ := s.InsertArtifact(domain.ArtifactRecord{
		SessionID: "ses", Content: "new", TotalChars: 3, TotalBytes: 3, CreatedAt: gcNOW - 1000,
	})

	if err := s.GCRetentionSweep(gcNOW); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetArtifact(old.ID); ok {
		t.Fatal("ancient artifact past the window should be swept")
	}
	if _, ok := s.GetArtifact(recent.ID); !ok {
		t.Fatal("recent artifact must be retained")
	}
}
