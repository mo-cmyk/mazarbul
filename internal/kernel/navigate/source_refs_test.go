package navigate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// TestGetSourceRef seeds a personal-scope source_refs row and exercises the
// happy path, the not-found path, and the #206 scope boundary: an empty scope
// set fails closed, and a row outside the requested scope is invisible.
func TestGetSourceRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, exec := openNavStore(t)

	const ins = `INSERT INTO source_refs
		(uid, subject_uid, source_type, connector, stream, external_id, external_url,
		 content_hash, fetch_time, dedup_key, source_event_uid, create_time, scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
	exec(ins, "sr1", "subj1", "email", "gmail", "INBOX", "msg-42",
		"https://mail.example.com/msg-42", "", "2026-06-15T10:00:00Z", "gmail:msg-42",
		"ev1", "2026-06-15T10:00:00Z", "personal")

	nav, err := navigate.New(store.Reader()) // defaults to AllScopes
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}

	// Happy path: the row is fetched with its fields and AIP name.
	sr, err := nav.GetSourceRef(ctx, "sr1")
	if err != nil {
		t.Fatalf("GetSourceRef: %v", err)
	}
	if sr.Name != "sourceRefs/sr1" {
		t.Errorf("Name = %q, want sourceRefs/sr1", sr.Name)
	}
	if sr.SourceType != "email" || sr.ExternalURL != "https://mail.example.com/msg-42" {
		t.Errorf("row = %+v, want email + verbatim url", sr)
	}
	if sr.DedupKey != "gmail:msg-42" || sr.Scope != "personal" {
		t.Errorf("row = %+v, want dedup gmail:msg-42 + personal scope", sr)
	}

	// Not found.
	if _, err := nav.GetSourceRef(ctx, "missing"); !errors.Is(err, navigate.ErrNotFound) {
		t.Errorf("GetSourceRef(missing): want ErrNotFound, got %v", err)
	}

	// Fail-closed: an empty scope set admits zero rows (#206), so even an
	// existing uid is not found.
	empty := nav.WithScopes(kernel.ScopeSet{})
	if _, err := empty.GetSourceRef(ctx, "sr1"); !errors.Is(err, navigate.ErrNotFound) {
		t.Errorf("GetSourceRef under empty scope: want ErrNotFound (fail-closed), got %v", err)
	}

	// Scope isolation: the personal row is invisible to a team-only read.
	team := nav.WithScopes(kernel.NewScopeSet(kernel.ScopeTeam))
	if _, err := team.GetSourceRef(ctx, "sr1"); !errors.Is(err, navigate.ErrNotFound) {
		t.Errorf("GetSourceRef of a personal row under team scope: want ErrNotFound, got %v", err)
	}
}
