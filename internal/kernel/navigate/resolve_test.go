package navigate_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// openResolveStore opens a fresh SQLite store and returns a seeded Navigator
// for the ResolveDisplayName tests. Helper factored out so each sub-test gets
// its own isolated DB.
func openResolveStore(t *testing.T, seed func(exec func(string, ...any))) *navigate.Navigator {
	t.Helper()
	store, exec := openNavStore(t)
	seed(exec)
	nav, err := navigate.New(store.Reader())
	if err != nil {
		t.Fatalf("navigate.New: %v", err)
	}
	return nav
}

// resolveNow returns the current UTC time formatted as RFC3339Nano for entity seeding.
func resolveNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// seedEntity inserts one entity row via the exec helper.
func seedEntity(exec func(string, ...any), uid, kind, name string) {
	exec(
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`,
		uid, kind, name, 1.0, "confirmed", resolveNow(),
	)
}

// TestResolveDisplayName_PrefersNonDocument verifies that when both a
// non-document entity and a document share an exact display name, the
// non-document wins and the result carries ShadowedDocument=true with the
// shadowed document's identity in ShadowedDocuments.
func TestResolveDisplayName_PrefersNonDocument(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "product-uid", "product", "Mithrilmail")
		seedEntity(exec, "doc-uid", "document", "Mithrilmail")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Mithrilmail", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK", result.Status)
	}
	if result.UID != "product-uid" {
		t.Fatalf("uid = %q, want product-uid", result.UID)
	}
	if !result.ShadowedDocument {
		t.Fatalf("ShadowedDocument = false, want true (document with same name was present)")
	}
	if len(result.ShadowedDocuments) != 1 {
		t.Fatalf("ShadowedDocuments len = %d, want 1", len(result.ShadowedDocuments))
	}
	if result.ShadowedDocuments[0].UID != "doc-uid" {
		t.Fatalf("ShadowedDocuments[0].UID = %q, want doc-uid", result.ShadowedDocuments[0].UID)
	}
	if result.ShadowedDocuments[0].Kind != "document" {
		t.Fatalf("ShadowedDocuments[0].Kind = %q, want document", result.ShadowedDocuments[0].Kind)
	}
}

// TestResolveDisplayName_MultipleNonDocAmbiguous verifies that two non-document
// entities with the same display name and same kind return ResolvedAmbiguous.
func TestResolveDisplayName_MultipleNonDocAmbiguous(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "u1", "person", "Sam")
		seedEntity(exec, "u2", "person", "Sam")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Sam", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedAmbiguous {
		t.Fatalf("status = %v, want ResolvedAmbiguous", result.Status)
	}
	if len(result.Candidates) < 2 {
		t.Fatalf("candidates len = %d, want >= 2", len(result.Candidates))
	}
}

// TestResolveDisplayName_KindHint selects only the document when kindHint is
// "document", ignoring the product entity with the same name.
func TestResolveDisplayName_KindHint(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "product-uid", "product", "Mithrilmail")
		seedEntity(exec, "doc-uid", "document", "Mithrilmail")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Mithrilmail", navigate.ResolveStrict, "document")
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK", result.Status)
	}
	if result.UID != "doc-uid" {
		t.Fatalf("uid = %q, want doc-uid", result.UID)
	}
}

// TestResolveDisplayName_NotFound verifies that a name with no matching entity
// returns ResolvedNotFound.
func TestResolveDisplayName_NotFound(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "u1", "person", "Sam")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Nobody Here", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedNotFound {
		t.Fatalf("status = %v, want ResolvedNotFound", result.Status)
	}
	if result.UID != "" {
		t.Fatalf("uid = %q, want empty on not-found", result.UID)
	}
}

// TestResolveDisplayName_SingleNonDocNoDocument verifies that a lone
// non-document entity resolves without ShadowedDocument when no document
// namesake exists.
func TestResolveDisplayName_SingleNonDocNoDocument(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "p1", "person", "Rosie Cotton")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Rosie Cotton", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK", result.Status)
	}
	if result.UID != "p1" {
		t.Fatalf("uid = %q, want p1", result.UID)
	}
	if result.ShadowedDocument {
		t.Fatalf("ShadowedDocument = true, want false when no doc namesake")
	}
}

// TestResolveDisplayName_ResolveDangle verifies that ResolveDangle returns
// ResolvedAmbiguous (not an error) when two non-document entities share a name.
func TestResolveDisplayName_ResolveDangle(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "u1", "person", "Sam")
		seedEntity(exec, "u2", "person", "Sam")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Sam", navigate.ResolveDangle, "")
	if result.Status != navigate.ResolvedAmbiguous {
		t.Fatalf("status = %v, want ResolvedAmbiguous in dangle mode", result.Status)
	}
}

// TestResolveDisplayName_KindPriorityTieBreak verifies that when two different
// non-document kinds share a name, the higher-priority kind wins deterministically.
func TestResolveDisplayName_KindPriorityTieBreak(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "org-uid", "organization", "Erebor")
		seedEntity(exec, "prod-uid", "product", "Erebor")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Erebor", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK (organization wins over product)", result.Status)
	}
	if result.UID != "org-uid" {
		t.Fatalf("uid = %q, want org-uid (organization has higher priority than product)", result.UID)
	}
}

// TestResolveDisplayName_CrossResolver seeds one person "Rosie Cotton" and one
// document titled "Rosie Cotton" and verifies that the shared resolver picks
// the person for both the ResolveStrict path (MCP) and the ResolveDangle path
// (wikilinks). This is the cross-resolver integration test required by spec.
func TestResolveDisplayName_CrossResolver(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "person-uid", "person", "Rosie Cotton")
		seedEntity(exec, "doc-uid", "document", "Rosie Cotton")
	})
	ctx := context.Background()

	t.Run("MCP strict picks person", func(t *testing.T) {
		t.Parallel()
		result := nav.ResolveDisplayName(ctx, "Rosie Cotton", navigate.ResolveStrict, "")
		if result.Status != navigate.ResolvedOK {
			t.Fatalf("strict: status = %v, want ResolvedOK", result.Status)
		}
		if result.UID != "person-uid" {
			t.Fatalf("strict: uid = %q, want person-uid", result.UID)
		}
		if !result.ShadowedDocument {
			t.Fatalf("strict: ShadowedDocument = false, want true")
		}
	})

	t.Run("Wikilink dangle picks person", func(t *testing.T) {
		t.Parallel()
		result := nav.ResolveDisplayName(ctx, "Rosie Cotton", navigate.ResolveDangle, "")
		if result.Status != navigate.ResolvedOK {
			t.Fatalf("dangle: status = %v, want ResolvedOK", result.Status)
		}
		if result.UID != "person-uid" {
			t.Fatalf("dangle: uid = %q, want person-uid", result.UID)
		}
	})
}

// TestResolveDisplayName_DocOnlyNoCandidates verifies that when only a document
// entity exists for a given name (no non-document), it resolves to it.
func TestResolveDisplayName_DocOnlyNoCandidates(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "doc-uid", "document", "Meeting Notes")
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Meeting Notes", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK (only doc exists)", result.Status)
	}
	if result.UID != "doc-uid" {
		t.Fatalf("uid = %q, want doc-uid", result.UID)
	}
}

// seedDocumentWithCanonical seeds an entity of kind "document" and the
// companion documents row, setting canonical to the given value (1 = true,
// 0 = false). Path is derived from uid to satisfy the UNIQUE constraint.
func seedDocumentWithCanonical(exec func(string, ...any), uid, name string, canonical int) {
	now := resolveNow()
	exec(
		`INSERT INTO entities (uid, kind, display_name, confidence, resolution_status, create_time) VALUES (?,?,?,?,?,?)`,
		uid, "document", name, 1.0, "confirmed", now,
	)
	// content_hash and revision_uid have no DEFAULT — supply fixed values.
	// The 64-character hex string satisfies the SHA-256 pattern without
	// running a real hash.
	const fakeHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	exec(
		`INSERT INTO documents (document_uid, path, content_hash, revision_uid, canonical, create_time, update_time) VALUES (?,?,?,?,?,?,?)`,
		uid, "notes/"+uid+".md", fakeHash, "rev-"+uid, canonical, now, now,
	)
}

// TestResolveDisplayName_CanonicalDocWins verifies guardrail G5: a document
// entity whose documents row carries canonical=1 wins same-name resolution over
// a non-document entity, even though the default policy prefers non-docs.
func TestResolveDisplayName_CanonicalDocWins(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "product-uid", "product", "Mithrilmail")
		seedDocumentWithCanonical(exec, "doc-uid", "Mithrilmail", 1) // canonical:true
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Mithrilmail", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK (canonical doc must win)", result.Status)
	}
	if result.UID != "doc-uid" {
		t.Fatalf("uid = %q, want doc-uid (canonical document wins over non-document)", result.UID)
	}
	// ShadowedDocument is not set when the doc wins; it is only set when a
	// non-doc wins and a doc is present as a namesake.
	if result.ShadowedDocument {
		t.Fatalf("ShadowedDocument = true, want false (the doc is the winner, not shadowed)")
	}
}

// TestResolveDisplayName_NonCanonicalDocLosesToEntity is a regression guard:
// a document entity whose documents row carries canonical=0 (the default) must
// NOT win over a non-document entity — the existing pre-G5 behaviour is
// preserved unchanged.
func TestResolveDisplayName_NonCanonicalDocLosesToEntity(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		seedEntity(exec, "product-uid", "product", "Mithrilmail")
		seedDocumentWithCanonical(exec, "doc-uid", "Mithrilmail", 0) // canonical:false
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Mithrilmail", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedOK {
		t.Fatalf("status = %v, want ResolvedOK (non-canonical doc must not shadow entity)", result.Status)
	}
	if result.UID != "product-uid" {
		t.Fatalf("uid = %q, want product-uid (non-canonical doc must lose to non-doc entity)", result.UID)
	}
	if !result.ShadowedDocument {
		t.Fatalf("ShadowedDocument = false, want true (doc is present but shadowed by the entity winner)")
	}
}

// TestResolveDisplayName_ExactNameCandidateCap verifies that more than
// exactNameResolveCap same-name entities triggers the overflow path and
// returns ResolvedAmbiguous without resolving to any single entry.
func TestResolveDisplayName_ExactNameCandidateCap(t *testing.T) {
	t.Parallel()
	nav := openResolveStore(t, func(exec func(string, ...any)) {
		for i := range 52 {
			seedEntity(exec, fmt.Sprintf("u%03d", i), "product", "Common Name")
		}
	})
	ctx := context.Background()
	result := nav.ResolveDisplayName(ctx, "Common Name", navigate.ResolveStrict, "")
	if result.Status != navigate.ResolvedAmbiguous {
		t.Fatalf("status = %v, want ResolvedAmbiguous on overflow", result.Status)
	}
}
