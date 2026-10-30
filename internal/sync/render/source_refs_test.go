package render_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/sync/render"
)

// uriHash returns the lower-hex SHA-256 of s, mirroring the render package's
// hashExternalID function. Used in redaction assertions below.
func uriHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestRender_SourceRef_PlaintextRedaction asserts the #207 render rule: in the
// plaintext push projection, external_url is omitted entirely and external_id
// is replaced by its SHA-256 hash. The subject/uid stay in the clear so the
// provenance lineage is still followable.
func TestRender_SourceRef_PlaintextRedaction(t *testing.T) {
	t.Parallel()

	uid := ulidAt(1)
	subjectUID := ulidAt(2)
	const externalID = "user:12345"
	const externalURL = "https://connector.example.com/user/12345"

	ev := makeEvent(uid, kernel.EventKind(ontology.KindSourceRefRecorded),
		ontology.SourceRefRecordedEvent{
			UID:         ontology.ULID(uid),
			SubjectUID:  ontology.ULID(subjectUID),
			SourceType:  "email",
			Connector:   "gmail",
			ExternalID:  externalID,
			ExternalURL: externalURL,
			FetchTime:   baseTime,
			DedupKey:    "gmail:user:12345",
		})

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopePersonal,
		Events: seqEvents([]kernel.Event{ev}),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	f := fileByPath(view.Files, "rendered/source-refs.jsonl")
	if f == nil {
		t.Fatal("rendered/source-refs.jsonl not produced")
	}

	// external_url must be omitted: neither the value nor the field key appears.
	if bytes.Contains(f.Content, []byte(externalURL)) {
		t.Errorf("external_url value leaked into plaintext output:\n%s", f.Content)
	}
	if bytes.Contains(f.Content, []byte("externalUrl")) {
		t.Errorf("externalUrl field key present in plaintext output (should be omitted):\n%s", f.Content)
	}
	// dedup_key is omitted entirely: it commonly embeds the external id (here
	// "gmail:user:12345"), so emitting it would re-leak the hashed identifier.
	if bytes.Contains(f.Content, []byte("dedupKey")) {
		t.Errorf("dedupKey field present in plaintext output (should be omitted):\n%s", f.Content)
	}
	// external_id must be hashed: the raw value is absent (even though the
	// source dedupKey contained it), its SHA-256 present.
	if bytes.Contains(f.Content, []byte(externalID)) {
		t.Errorf("raw external_id leaked into plaintext output:\n%s", f.Content)
	}
	sum := sha256.Sum256([]byte(externalID))
	wantHash := hex.EncodeToString(sum[:])
	if !bytes.Contains(f.Content, []byte(wantHash)) {
		t.Errorf("external_id hash %q not found in output:\n%s", wantHash, f.Content)
	}
	// Lineage stays followable: uid + subjectUid in the clear. The emitted uid is
	// the dedupKey-derived identity (matching the source_refs projection), not the
	// payload uid, so the rendered file and the queryable row share one identity.
	derivedUID := string(ontology.SourceRefUID("gmail:user:12345"))
	if !bytes.Contains(f.Content, []byte(derivedUID)) {
		t.Errorf("derived uid %q missing from output:\n%s", derivedUID, f.Content)
	}
	if bytes.Contains(f.Content, []byte(uid)) {
		t.Errorf("payload uid %q leaked into output; identity must be the dedupKey-derived uid:\n%s", uid, f.Content)
	}
	if !bytes.Contains(f.Content, []byte(subjectUID)) {
		t.Errorf("subjectUid missing from output:\n%s", f.Content)
	}
}

// TestRender_SourceRef_DedupCollapsesToOneLine asserts that two events sharing a
// dedup_key (which derive the same uid) render as ONE line with last-write-wins
// values — matching the source_refs UNIQUE(dedup_key) upsert. A per-event append
// would emit two duplicate-uid lines and render non-deterministically.
func TestRender_SourceRef_DedupCollapsesToOneLine(t *testing.T) {
	t.Parallel()

	const dedupKey = "gmail:dup-1"
	mk := func(evUID, externalID string) kernel.Event {
		return makeEvent(evUID, kernel.EventKind(ontology.KindSourceRefRecorded),
			ontology.SourceRefRecordedEvent{
				UID:        ontology.SourceRefUID(dedupKey),
				SubjectUID: ontology.ULID(ulidAt(2)),
				SourceType: "email",
				ExternalID: externalID,
				FetchTime:  baseTime,
				DedupKey:   dedupKey,
			})
	}
	// Two distinct events sharing a dedupKey; the second changes externalId.
	view, err := render.Render(render.Input{
		Scope:  kernel.ScopePersonal,
		Events: seqEvents([]kernel.Event{mk(ulidAt(0), "first"), mk(ulidAt(1), "second")}),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f := fileByPath(view.Files, "rendered/source-refs.jsonl")
	if f == nil {
		t.Fatal("source-refs.jsonl not produced")
	}
	if n := bytes.Count(f.Content, []byte("\n")); n != 1 {
		t.Errorf("rendered %d lines, want 1 (dedup collapse):\n%s", n, f.Content)
	}
	// Last-write-wins: the second event's externalId hash is present, the first's absent.
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	if !bytes.Contains(f.Content, []byte(sum("second"))) {
		t.Errorf("last-write-wins violated: second externalId hash absent:\n%s", f.Content)
	}
	if bytes.Contains(f.Content, []byte(sum("first"))) {
		t.Errorf("stale first externalId hash present after re-record:\n%s", f.Content)
	}
}

// TestRender_SourceRef_Deterministic asserts a second render of the same event
// stream produces byte-identical source-refs.jsonl.
func TestRender_SourceRef_Deterministic(t *testing.T) {
	t.Parallel()

	ev := makeEvent(ulidAt(1), kernel.EventKind(ontology.KindSourceRefRecorded),
		ontology.SourceRefRecordedEvent{
			UID:        ontology.ULID(ulidAt(1)),
			SubjectUID: ontology.ULID(ulidAt(2)),
			SourceType: "web_page",
			ExternalID: "abc",
			FetchTime:  baseTime,
			DedupKey:   "web:abc",
		})

	render1, err := render.Render(render.Input{Scope: kernel.ScopePersonal, Events: seqEvents([]kernel.Event{ev})})
	if err != nil {
		t.Fatalf("Render (1): %v", err)
	}
	render2, err := render.Render(render.Input{Scope: kernel.ScopePersonal, Events: seqEvents([]kernel.Event{ev})})
	if err != nil {
		t.Fatalf("Render (2): %v", err)
	}

	f1 := fileByPath(render1.Files, "rendered/source-refs.jsonl")
	f2 := fileByPath(render2.Files, "rendered/source-refs.jsonl")
	if f1 == nil || f2 == nil {
		t.Fatal("source-refs.jsonl missing from one render")
	}
	if !bytes.Equal(f1.Content, f2.Content) {
		t.Errorf("non-deterministic output:\n  %s\n  %s", f1.Content, f2.Content)
	}
}

// TestRender_SourceRef_ControlCharsAreValidJSON asserts that control characters
// in free-text fields (here stream) are emitted as valid JSON escapes. The
// earlier fmt.Sprintf+%q encoding produced Go-only escapes (\a, \v, \xNN) that
// are NOT valid JSON string escapes, so a single such byte would corrupt the
// whole line. json.Marshal emits \u00NN and round-trips exactly.
func TestRender_SourceRef_ControlCharsAreValidJSON(t *testing.T) {
	t.Parallel()

	// Bell (\a, 0x07), vertical tab (\v, 0x0b) and unit separator (0x1f) are the
	// escapes %q renders Go-style but JSON cannot parse.
	const stream = "alert\x07vtab\x0bunit\x1f"

	ev := makeEvent(ulidAt(1), kernel.EventKind(ontology.KindSourceRefRecorded),
		ontology.SourceRefRecordedEvent{
			UID:        ontology.SourceRefUID("ctl:1"),
			SubjectUID: ontology.ULID(ulidAt(2)),
			SourceType: "email",
			Stream:     stream,
			ExternalID: "x",
			FetchTime:  baseTime,
			DedupKey:   "ctl:1",
		})

	view, err := render.Render(render.Input{
		Scope:  kernel.ScopePersonal,
		Events: seqEvents([]kernel.Event{ev}),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f := fileByPath(view.Files, "rendered/source-refs.jsonl")
	if f == nil {
		t.Fatal("rendered/source-refs.jsonl not produced")
	}

	line := bytes.TrimRight(f.Content, "\n")
	if !json.Valid(line) {
		t.Fatalf("emitted line is not valid JSON:\n%s", line)
	}
	var got struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unmarshal emitted line: %v\nline: %s", err, line)
	}
	if got.Stream != stream {
		t.Errorf("stream round-trip mismatch:\n got  %q\n want %q", got.Stream, stream)
	}
}

// TestRender_ExternalRefEntity_URIRedacted asserts the entity-level plaintext
// render rule: for an external_ref entity whose uri-type identifier value and
// display_name both equal the raw external URL, the renderer replaces both with
// the SHA-256 hash in every emitted file (entity stub, entities.jsonl, and the
// CHANGELOG fragment). The raw URL must not appear anywhere in the output.
//
// Redaction is UNCONDITIONAL (same rationale as the source_refs #207 render rule
// in writeSourceRefsJSONL): the renderer only ever runs on the plaintext push
// path, so there is no scope-branch to guard.
func TestRender_ExternalRefEntity_URIRedacted(t *testing.T) {
	t.Parallel()

	const rawURI = "https://notion.so/p/abc123-workspaceid-slug"
	uid := ulidAt(3000)
	headUID := ulidAt(3001)

	// Entity with kind=external_ref, a {type:"uri"} identifier, and a
	// display_name that matches the URI (the reference() tool default).
	entityEv := makeEvent(uid, ontology.KindEntity, ontology.Entity{
		Name:             "entities/" + uid,
		UID:              uid,
		Kind:             ontology.EntityKindExternalRef,
		DisplayName:      rawURI,
		Confidence:       1.0,
		ResolutionStatus: ontology.ResolutionConfirmed,
		CreateTime:       baseTime,
		Identifiers: []ontology.Identifier{
			{Type: "uri", Value: rawURI},
		},
	})

	view, err := render.Render(render.Input{
		Scope:    kernel.ScopePersonal,
		Events:   seqEvents([]kernel.Event{entityEv}),
		HeadULID: headUID,
		Device:   "test-device",
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// Primary assertion: the raw URI must be absent from every rendered file.
	for _, f := range view.Files {
		if bytes.Contains(f.Content, []byte(rawURI)) {
			t.Errorf("raw URI leaked into %s:\n%s", f.Path, f.Content)
		}
	}

	want := uriHash(rawURI)

	// Entity stub must contain the hash in both the H1 heading and the
	// identifiers frontmatter block.
	stubPath := "rendered/by-uid/" + uid[:3] + "/" + uid[3:5] + "/" + uid + ".md"
	stub := fileByPath(view.Files, stubPath)
	if stub == nil {
		t.Fatalf("entity stub not found at %q", stubPath)
	}
	if !bytes.Contains(stub.Content, []byte(want)) {
		t.Errorf("URI hash %q absent from entity stub:\n%s", want, stub.Content)
	}

	// entities.jsonl must carry the hash in both displayName and the identifier value.
	entJSONL := fileByPath(view.Files, "rendered/entities.jsonl")
	if entJSONL == nil {
		t.Fatal("entities.jsonl not found")
	}
	if !bytes.Contains(entJSONL.Content, []byte(want)) {
		t.Errorf("URI hash %q absent from entities.jsonl:\n%s", want, entJSONL.Content)
	}

	// Sanity: verify kind=external_ref is present (proving the entity was rendered).
	if !bytes.Contains(entJSONL.Content, []byte(`"external_ref"`)) {
		t.Errorf("entities.jsonl missing kind=external_ref; got:\n%s", entJSONL.Content)
	}
}
