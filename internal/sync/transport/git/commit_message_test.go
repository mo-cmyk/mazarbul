package git

// commit_message_test.go pins the contract for the structured sync
// commit messages: a one-line summary that fits `git log --oneline`,
// a parseable body in plaintext mode, NO payload details in encrypted
// mode, and a stable `Mazarbul-Schema:` trailer so blame/changelog
// parsers can version-gate.

import (
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// onelineMax is the column git's --oneline default truncates at on
// most terminals. Keeping subjects within this cap is the difference
// between a scannable `git log --oneline` and a wrapped mess.
const onelineMax = 72

func sampleSummary() pushSummary {
	heads := []chunk.Header{
		{ChunkID: "01KT8X4KCDE22Z5RBJZVX626EM", Scope: kernel.ScopePersonal, Count: 8, FirstULID: "01KT8X01K08T810JEPT56PK0MH", LastULID: "01KT8X3FN3SY45FVSCR29E796E", Encrypted: false},
	}
	details := &commitDetails{
		Episodes: []episodeRef{
			{UID: "01KT8X01K08T810JEPT56PK0MH", Snippet: "Frodo is the CEO of Erebor based in Berlin."},
			{UID: "01KT8X30X9X460TKXCK659FYW3", Snippet: "Mazarbul is a local-first agent system of record written in Go."},
			{UID: "01KT8X3FN3SY45FVSCR29E796E", Snippet: "The hub was deleted in PR 144."},
		},
		Entities: []entityChange{
			{UID: "01KT8X0SERT5FJB0VGPYQQGAEN", Kind: "person", DisplayName: "Frodo", Action: "+"},
			{UID: "01KT8X0SKKTG80B04FEQHQ7PRE", Kind: "organization", DisplayName: "Erebor", Action: "+"},
			{UID: "01KT8X0SR8METWBDCW26ZV8BW1", Kind: "place", DisplayName: "Berlin", Action: "+"},
			{UID: "01KT8X3N5HMW1E880E147GXDYS", Kind: "project", DisplayName: "PR 144", Action: "+"},
			{UID: "self", Kind: "person", DisplayName: "Me", Action: "~"},
		},
		Relations: []relationChange{
			{SubjectUID: "01KT8X0SERT5FJB0VGPYQQGAEN", Predicate: "ceo_of", ObjectUID: "01KT8X0SKKTG80B04FEQHQ7PRE", Confidence: 1.0, Action: "+"},
			{SubjectUID: "01KT8X0SKKTG80B04FEQHQ7PRE", Predicate: "located_in", ObjectUID: "01KT8X0SR8METWBDCW26ZV8BW1", Confidence: 1.0, Action: "+"},
		},
	}
	return pushSummary{
		Scope:     kernel.ScopePersonal,
		DeviceID:  "maz-sync-cli",
		Tally:     pushTally{plaintext: 1},
		Heads:     heads,
		SizeBytes: 28_100,
		Details:   details,
	}
}

func TestBuildCommitMessage_SubjectFitsOneline(t *testing.T) {
	msg := buildCommitMessage(sampleSummary())
	subject := firstLine(msg)
	if len(subject) > onelineMax {
		t.Fatalf("subject is %d chars (> %d): %q", len(subject), onelineMax, subject)
	}
	if !strings.HasPrefix(subject, "sync(personal):") {
		t.Errorf("subject should start with `sync(<scope>):`, got %q", subject)
	}
}

func TestBuildCommitMessage_SubjectCarriesSemanticCounts(t *testing.T) {
	subject := firstLine(buildCommitMessage(sampleSummary()))
	for _, want := range []string{"+3 episodes", "+5 entities", "+2 relations"} {
		if !strings.Contains(subject, want) {
			t.Errorf("subject %q missing %q", subject, want)
		}
	}
}

func TestWideBatchSubject(t *testing.T) {
	// A `recreate`/`rebuild` re-commits the whole store in one commit, so the
	// full count line overflows the 72-char budget. The subject must stay
	// under budget while keeping the leading headline figures next to a
	// "(N chunks)" suffix — not collapse to a bare chunk count.
	heads := make([]chunk.Header, 16)
	d := &commitDetails{
		Episodes:  make([]episodeRef, 3),
		Entities:  make([]entityChange, 1234),
		Documents: make([]documentChange, 567),
		Relations: make([]relationChange, 8910),
		Forgets:   make([]forgetRef, 42),
	}
	got := wideBatchSubject("sync(personal): ", pushSummary{Heads: heads, Details: d})
	if len(got) > onelineMax {
		t.Fatalf("wide-batch subject %d chars > %d: %q", len(got), onelineMax, got)
	}
	if !strings.HasSuffix(got, "(16 chunks)") {
		t.Errorf("wide-batch subject should report chunk count, got %q", got)
	}
	if !strings.HasPrefix(got, "sync(personal): +3 episodes") {
		t.Errorf("wide-batch subject dropped its leading counts, got %q", got)
	}

	// Encrypted / undecodable push has no details: fall back to bare count.
	bare := wideBatchSubject("sync(team): ", pushSummary{Heads: heads, Details: nil})
	if bare != "sync(team): (16 chunks)" {
		t.Errorf("encrypted wide-batch = %q, want bare chunk count", bare)
	}
}

func TestBuildCommitMessage_PlaintextBodyEnumeratesEverything(t *testing.T) {
	body := buildCommitMessage(sampleSummary())
	for _, want := range []string{
		"Episodes:",
		"01KT8X01K08T810JEPT56PK0MH",
		"Frodo is the CEO of Erebor based in Berlin.",
		"Entities:",
		"+ person/01KT8X0SERT5FJB0VGPYQQGAEN",
		"Frodo",
		"~ person/self",
		"Relations:",
		"--[ceo_of]-->",
		"conf 1.00",
		"Chunks: 1 plaintext",
		"Device: maz-sync-cli",
		"Mazarbul-Schema: sync-commit/v1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestBuildCommitMessage_EncryptedModeNeverLeaksPayloads(t *testing.T) {
	s := sampleSummary()
	s.Tally = pushTally{encrypted: 3}
	s.Details = nil // privacy contract: encrypted-mode caller passes no details
	s.Heads[0].Encrypted = true

	msg := buildCommitMessage(s)
	subject := firstLine(msg)
	if len(subject) > onelineMax {
		t.Errorf("encrypted subject %d chars > %d: %q", len(subject), onelineMax, subject)
	}
	if !strings.Contains(subject, "sealed") && !strings.Contains(subject, "encrypted") {
		t.Errorf("encrypted subject should mark itself as sealed, got %q", subject)
	}

	// The leak invariant: NO entity name, predicate, or payload text
	// may appear anywhere in the message, even if the caller mistakenly
	// passed details. Test what we get when caller correctly passes nil.
	forbidden := []string{"Frodo", "Erebor", "Berlin", "ceo_of", "located_in", "Mazarbul is a local", "PR 144"}
	for _, leak := range forbidden {
		if strings.Contains(msg, leak) {
			t.Errorf("encrypted commit leaked %q\n--- msg ---\n%s", leak, msg)
		}
	}
	if !strings.Contains(msg, "Mazarbul-Schema: sync-commit/v1") {
		t.Errorf("encrypted commit missing schema trailer\n%s", msg)
	}
}

func TestBuildCommitMessage_TruncatesLargeSections(t *testing.T) {
	s := sampleSummary()
	for i := range 200 {
		s.Details.Entities = append(s.Details.Entities, entityChange{
			UID:         "01KT8X000000000000000000" + string(rune('A'+i%26)),
			Kind:        "person",
			DisplayName: "Person" + string(rune('A'+i%26)),
			Action:      "+",
		})
	}
	msg := buildCommitMessage(s)
	if !strings.Contains(msg, "and ") || !strings.Contains(msg, " more") {
		t.Errorf("expected `… and N more` truncation marker; not present")
	}
	// Total length stays sensible — well under 50 KiB even with 200
	// entities, otherwise a `maz rebuild` would produce multi-MB
	// commits.
	if len(msg) > 50_000 {
		t.Errorf("commit message %d bytes > 50 KiB; truncation policy is broken", len(msg))
	}
}

func TestBuildCommitMessage_DegradesWhenDetailsAbsent(t *testing.T) {
	// Plaintext chunks were pushed but the caller couldn't decode them
	// (e.g. a future on-disk schema bump the transport doesn't yet
	// understand). The commit should still produce a sane subject and
	// the schema trailer, just without the structured body.
	s := sampleSummary()
	s.Details = nil
	msg := buildCommitMessage(s)
	if !strings.Contains(msg, "Mazarbul-Schema: sync-commit/v1") {
		t.Errorf("missing schema trailer in degraded mode\n%s", msg)
	}
	if strings.Contains(msg, "Episodes:") {
		t.Errorf("degraded mode should not invent body sections\n%s", msg)
	}
}

func TestParseStructuredCommit_PerEntityAction(t *testing.T) {
	msg := buildCommitMessage(sampleSummary())
	// Frodo appears as `+ person/01KT8X0SERT5...  Frodo` — blame should
	// pull "added" for that UID.
	got := actionForEntity(msg, "01KT8X0SERT5FJB0VGPYQQGAEN")
	if got != "added" {
		t.Errorf("actionForEntity(Frodo) = %q, want \"added\"", got)
	}
	// self had `~` prefix → "updated"
	got = actionForEntity(msg, "self")
	if got != "updated" {
		t.Errorf("actionForEntity(self) = %q, want \"updated\"", got)
	}
	// Entity not in this commit — caller should see the neutral fallback.
	got = actionForEntity(msg, "01HHHHNOTPRESENT0000000000")
	if got != "modified" {
		t.Errorf("actionForEntity(absent) = %q, want \"modified\"", got)
	}
}

func TestDedupeAndResolve_CollapsesRepeatEntitiesAndNamesRelations(t *testing.T) {
	d := &commitDetails{
		Entities: []entityChange{
			{UID: "01KT8X0SERT5FJB0VGPYQQGAEN", Kind: "person", DisplayName: "Frodo", Action: "+"},
			{UID: "01KT8X0SERT5FJB0VGPYQQGAEN", Kind: "person", DisplayName: "Frodo", Action: "+"},
			{UID: "01KT8X0SERT5FJB0VGPYQQGAEN", Kind: "person", DisplayName: "Frodo", Action: "+"},
			{UID: "01KT8X0SKKTG80B04FEQHQ7PRE", Kind: "organization", DisplayName: "Erebor", Action: "+"},
		},
		Relations: []relationChange{
			{SubjectUID: "01KT8X0SERT5FJB0VGPYQQGAEN", Predicate: "ceo_of", ObjectUID: "01KT8X0SKKTG80B04FEQHQ7PRE", Confidence: 1.0, Action: "+"},
		},
	}
	dedupeAndResolve(d)
	if len(d.Entities) != 2 {
		t.Errorf("entities not deduped: got %d, want 2: %+v", len(d.Entities), d.Entities)
	}
	if !strings.Contains(d.Relations[0].SubjectUID, "Frodo") {
		t.Errorf("subject UID not resolved to display name: %q", d.Relations[0].SubjectUID)
	}
	if !strings.Contains(d.Relations[0].ObjectUID, "Erebor") {
		t.Errorf("object UID not resolved to display name: %q", d.Relations[0].ObjectUID)
	}
	if !strings.Contains(d.Relations[0].SubjectUID, "01KT8X0S") {
		t.Errorf("short UID hint missing from resolved subject: %q", d.Relations[0].SubjectUID)
	}
}

func TestSubjectCountsUniqueEntities(t *testing.T) {
	// The subject's "+N entities" must reflect the deduped count, not
	// the raw-event count, otherwise a `git log --oneline` of a fresh
	// remember reads "+14 entities" when only 4 unique entities landed.
	d := &commitDetails{
		Entities: []entityChange{
			{UID: "a", DisplayName: "A", Action: "+"},
			{UID: "a", DisplayName: "A", Action: "+"},
			{UID: "b", DisplayName: "B", Action: "+"},
		},
	}
	dedupeAndResolve(d)
	if got := subjectCountsPlaintext(d); !strings.Contains(got, "+2 entities") {
		t.Errorf("unique count not reflected in subject: got %q, want +2 entities", got)
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
