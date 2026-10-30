package warncode

import "testing"

func TestNew_FormatsMessageAndKeepsCode(t *testing.T) {
	w := New(CodeRecallEmbedderDegraded, "embedder timed out after %dms", 200)
	if w.Code != "MAZ-RECALL-001" {
		t.Errorf("Code = %q, want MAZ-RECALL-001", w.Code)
	}
	if w.Message != "embedder timed out after 200ms" {
		t.Errorf("Message = %q", w.Message)
	}
}

// TestCodes_AreUniqueAndWellFormed guards the append-only catalog: no two codes
// collide, and every code is MAZ-<AREA>-NNN shaped.
func TestCodes_AreUniqueAndWellFormed(t *testing.T) {
	codes := []string{
		CodeKindCloseMatch, CodeKindDedupDegraded,
		CodeSchemaSupersede, CodeSchemaOrphan, CodeSchemaCollapse,
		CodeRecallEmbedderDegraded, CodeRecallFuzzyFallback,
		CodeLoadDuplicateMerged, CodeLoadRecordSkipped,
		CodeFeedbackPromoted,
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate warning code %q", c)
		}
		seen[c] = true
		if len(c) < len("MAZ-X-001") || c[:4] != "MAZ-" {
			t.Errorf("code %q is not MAZ-<AREA>-NNN shaped", c)
		}
	}
}
