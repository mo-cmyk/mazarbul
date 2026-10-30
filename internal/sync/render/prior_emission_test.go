package render

import (
	"encoding/json"
	"iter"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// These are white-box (package render) tests: Render returns only a View, not
// the resulting GraphState, so a black-box test cannot capture a PriorState to
// feed the incremental path. Building state via the internal apply() mirrors
// exactly what a caller caches between renders.

func srEventForPrior(t *testing.T, seed, externalID, dedupKey string) kernel.Event {
	t.Helper()
	payload, err := json.Marshal(ontology.SourceRefRecordedEvent{
		UID:        ontology.SourceRefUID(dedupKey),
		SubjectUID: ontology.SourceRefUID("subject-seed"),
		SourceType: "email",
		ExternalID: externalID,
		FetchTime:  time.Unix(0, 0).UTC(),
		DedupKey:   dedupKey,
	})
	if err != nil {
		t.Fatalf("marshal sourceRefRecorded: %v", err)
	}
	return kernel.Event{
		UID:        string(ontology.SourceRefUID(seed)),
		Kind:       kernel.EventKind(ontology.KindSourceRefRecorded),
		CreateTime: time.Unix(0, 0).UTC(),
		Payload:    payload,
	}
}

func seqOfEvents(evs ...kernel.Event) iter.Seq[kernel.Event] {
	return func(yield func(kernel.Event) bool) {
		for _, e := range evs {
			if !yield(e) {
				return
			}
		}
	}
}

func findRenderedFile(files []File, path string) *File {
	for i := range files {
		if files[i].Path == path {
			return &files[i]
		}
	}
	return nil
}

// TestBuildPriorEmission_StablePathJSONLDiffsAsUnchanged asserts the fix for the
// Greptile finding: source-refs.jsonl has a stable path, so it must participate
// in the prior emission. Re-rendering identical state then skips it entirely
// (no redundant git write) instead of re-emitting ActionCreate every sync; a
// genuine change still surfaces as ActionUpdate.
func TestBuildPriorEmission_StablePathJSONLDiffsAsUnchanged(t *testing.T) {
	t.Parallel()

	const dedupKey = "gmail:msg-1"
	ev := srEventForPrior(t, "ev-1", "user:42", dedupKey)

	prior := newGraphState()
	if _, err := prior.apply(ev); err != nil {
		t.Fatalf("apply (prior): %v", err)
	}

	// Identical re-render: source-refs.jsonl content is byte-equal, so diffFiles
	// must skip it. Without the buildPriorEmission entry it would be ActionCreate.
	unchanged, err := Render(Input{
		Scope:      kernel.ScopePersonal,
		Events:     seqOfEvents(ev),
		PriorState: prior,
	})
	if err != nil {
		t.Fatalf("Render (unchanged): %v", err)
	}
	if f := findRenderedFile(unchanged.Files, renderedPath("source-refs.jsonl")); f != nil {
		t.Errorf("unchanged source-refs.jsonl re-emitted (Action=%v); want skipped", f.Action)
	}

	// Same dedupKey (→ same derived uid) but a new externalId: a real content
	// change must diff as ActionUpdate, not ActionCreate.
	ev2 := srEventForPrior(t, "ev-2", "user:99", dedupKey)
	changed, err := Render(Input{
		Scope:      kernel.ScopePersonal,
		Events:     seqOfEvents(ev2),
		PriorState: prior,
	})
	if err != nil {
		t.Fatalf("Render (changed): %v", err)
	}
	f := findRenderedFile(changed.Files, renderedPath("source-refs.jsonl"))
	if f == nil {
		t.Fatal("changed source-refs.jsonl missing from view")
	}
	if f.Action != ActionUpdate {
		t.Errorf("changed source-refs.jsonl Action = %v, want ActionUpdate", f.Action)
	}
}

// TestBuildPriorEmission_OmitsEmptyStablePathFiles guards the len>0 condition:
// an empty prior must NOT carry source-refs.jsonl/facts.jsonl keys, or diffFiles
// would see a prior path with no current counterpart and emit a phantom
// ActionDelete for a file that never existed.
func TestBuildPriorEmission_OmitsEmptyStablePathFiles(t *testing.T) {
	t.Parallel()

	em, err := buildPriorEmission(newGraphState())
	if err != nil {
		t.Fatalf("buildPriorEmission: %v", err)
	}
	for _, p := range []string{renderedPath("source-refs.jsonl"), renderedPath("facts.jsonl")} {
		if _, ok := em[p]; ok {
			t.Errorf("empty prior emission contains %q; would cause a spurious ActionDelete", p)
		}
	}
}
