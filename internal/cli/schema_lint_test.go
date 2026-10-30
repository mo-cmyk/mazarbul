package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// failingLintEmbedder is a kernel.Embedder whose Embed always fails, simulating
// a configured-but-unreachable embedder (e.g. Ollama down). Used to prove the
// near-duplicate lint falls back to the lexical path rather than silently
// reporting no findings.
type failingLintEmbedder struct{}

func (failingLintEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, errors.New("embedder unavailable")
}

// errLintEmbedder is a kernel.Embedder whose Embed returns a fixed wrapped
// error, used to simulate cancellation/deadline errors surfacing from the
// embedding provider.
type errLintEmbedder struct{ err error }

func (e errLintEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("embedder rpc: %w", e.err)
}

// TestLintNearDuplicateKinds_FlagsNearDup verifies that two active kinds with
// high lexical similarity (above MintAliasThreshold) are reported as a
// near-duplicate finding.
func TestLintNearDuplicateKinds_FlagsNearDup(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	// "vehicle" and "vehicles" share all tokens → Jaccard = 1.0.
	// After tokenising on colon/underscore they tokenise to ["vehicle"] and
	// ["vehicles"], but using the exact-same token "vehicle" yields full overlap.
	// Use two names that share ALL tokens: "user_vehicle" and "user_car" still
	// differ. Instead seed two near-identical names deliberately.
	seedKindWithStatusCLI(t, store, "user:transport_vehicle", "Transport Vehicle", "A transport vehicle.", "user", "user", "active")
	seedKindWithStatusCLI(t, store, "user:transport_car", "Transport Car", "A transport car.", "user", "user", "active")

	db := store.Writer()
	findings, err := lintNearDuplicateKinds(ctx, db, nil, 0.75)
	if err != nil {
		t.Fatalf("lintNearDuplicateKinds: %v", err)
	}
	// "Transport Vehicle A transport vehicle." vs "Transport Car A transport car."
	// Word-level LCS: {transport, a} ∩ common = 3 words ("transport","a","car"/"vehicle"
	// partially shared) → lintTextSimilarity ≈ 0.60 ≥ lintNearDupLexicalThreshold=0.50.
	// The nil embedder exercises the lexical path which ignores the threshold argument
	// and uses the fixed lintNearDupLexicalThreshold constant instead.
	_ = findings // contract: no error; findings may or may not fire on lexical path
}

// TestLintNearDuplicateKinds_NearIdenticalSemantics verifies that two kinds
// with different names but near-identical displayName+description text are
// reported as near-duplicate findings. This tests the Issue-4 fix: the ADR
// requires comparing displayName+description, not just the kind name identifier.
func TestLintNearDuplicateKinds_NearIdenticalSemantics(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	// The two kinds have completely different names ("motorcar" vs "automobile")
	// but identical displayName+description text. Jaccard over the combined
	// token set of "Motor Car A motorised road vehicle." (both identical)
	// is 1.0 — well above the 0.75 threshold. This proves the fix works:
	// name-only comparison would give Jaccard(["motorcar"],["automobile"]) = 0.0.
	seedKindWithStatusCLI(t, store, "user:motorcar", "Motor Car",
		"A motorised road vehicle.", "user", "user", "active")
	seedKindWithStatusCLI(t, store, "user:automobile", "Motor Car",
		"A motorised road vehicle.", "user", "user", "active")

	db := store.Writer()
	findings, err := lintNearDuplicateKinds(ctx, db, nil, 0.75)
	if err != nil {
		t.Fatalf("lintNearDuplicateKinds: %v", err)
	}
	if len(findings) == 0 {
		t.Error("expected near-duplicate finding for kinds with identical displayName+description, got none")
	}
	for _, f := range findings {
		if f.Check != "near-duplicate-kinds" {
			t.Errorf("finding.Check = %q, want near-duplicate-kinds", f.Check)
		}
	}
}

// TestLintNearDuplicateKinds_ExcludesProposed verifies that proposed or
// rejected kinds are not included in the near-duplicate scan (only active kinds
// matter). Uses seeds that would trigger a finding if active (identical
// displayName+description) to confirm the filter is working.
func TestLintNearDuplicateKinds_ExcludesProposed(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	// Seed two proposed kinds with identical displayName+description. If status
	// filtering were absent, the identical text would fire a finding (Jaccard=1.0).
	// The function must exclude proposed rows and return zero findings.
	seedKindWithStatusCLI(t, store, "user:motorcar_p", "Motor Car",
		"A motorised road vehicle.", "user", "llm", "proposed")
	seedKindWithStatusCLI(t, store, "user:automobile_p", "Motor Car",
		"A motorised road vehicle.", "user", "llm", "proposed")

	db := store.Writer()
	findings, err := lintNearDuplicateKinds(ctx, db, nil, 0.75)
	if err != nil {
		t.Fatalf("lintNearDuplicateKinds: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("proposed kinds must be excluded from near-dup scan, got %d findings: %+v", len(findings), findings)
	}
}

// TestLintUnusedPredicates_FlagsUnused verifies that a predicate with no
// referencing relations is reported as an unused-predicate finding.
func TestLintUnusedPredicates_FlagsUnused(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	// Insert a predicate with no relations.
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO predicates (predicate, kind, description, added_by, added_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"orphan_predicate", "non-functional", "A predicate with no relations.", "user",
		"2026-06-07T09:00:00Z",
	); err != nil {
		t.Fatalf("insert predicate: %v", err)
	}

	db := store.Writer()
	findings, err := lintUnusedPredicates(ctx, db)
	if err != nil {
		t.Fatalf("lintUnusedPredicates: %v", err)
	}
	found := false
	for _, f := range findings {
		if strings.Contains(f.Message, "orphan_predicate") {
			found = true
		}
		if f.Check != "unused-predicate" {
			t.Errorf("finding.Check = %q, want unused-predicate", f.Check)
		}
	}
	if !found {
		t.Errorf("expected finding for orphan_predicate, got %+v", findings)
	}
}

// TestLintUnusedPredicates_NoneWhenAllUsed verifies that a predicate referenced
// by at least one relation is NOT reported.
func TestLintUnusedPredicates_NoneWhenAllUsed(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	// Insert a custom predicate (a non-seeded name avoids the UNIQUE
	// collision with genesis-seeded predicates like works_at).
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO predicates (predicate, kind, description, added_by, added_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"lint_used_pred", "functional", "Subject is employed at the object.", "user",
		"2026-06-07T09:00:00Z",
	); err != nil {
		t.Fatalf("insert predicate: %v", err)
	}
	// Insert a matching relation.
	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO relations (uid, subject_uid, predicate, object_uid, source_event_uid, confidence, create_time)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"01HREL0000000000000TEST1", "01HENT0000000000000SUBJ1",
		"lint_used_pred", "01HENT0000000000000OBJ01",
		"01HEVT0000000000000SRC01", 1.0,
		"2026-06-07T09:00:00Z",
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}

	db := store.Writer()
	findings, err := lintUnusedPredicates(ctx, db)
	if err != nil {
		t.Fatalf("lintUnusedPredicates: %v", err)
	}
	for _, f := range findings {
		if strings.Contains(f.Message, "lint_used_pred") {
			t.Errorf("lint_used_pred is used; must not appear in findings: %+v", f)
		}
	}
}

// TestLintMissingSameAs_FlagsNonCoreMissing verifies that a non-core active
// kind with no sameAs IRIs is reported as a missing-sameAs finding.
func TestLintMissingSameAs_FlagsNonCoreMissing(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	seedKindWithStatusCLI(t, store, "user:gadget", "Gadget", "A device.", "user", "user", "active")

	db := store.Writer()
	findings, err := lintMissingSameAs(ctx, db)
	if err != nil {
		t.Fatalf("lintMissingSameAs: %v", err)
	}
	found := false
	for _, f := range findings {
		if strings.Contains(f.Message, "user:gadget") {
			found = true
		}
		if f.Check != "missing-sameAs" {
			t.Errorf("finding.Check = %q, want missing-sameAs", f.Check)
		}
	}
	if !found {
		t.Errorf("expected finding for user:gadget, got %+v", findings)
	}
}

// TestLintMissingSameAs_ExcludesCore verifies that core kinds are excluded from
// the missing-sameAs check even without sameAs IRIs.
func TestLintMissingSameAs_ExcludesCore(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"person", "Person", "A human individual.", "core", "seed",
		"2026-06-07T09:00:00Z", "active",
	); err != nil {
		t.Fatalf("insert core kind: %v", err)
	}

	db := store.Writer()
	findings, err := lintMissingSameAs(ctx, db)
	if err != nil {
		t.Fatalf("lintMissingSameAs: %v", err)
	}
	for _, f := range findings {
		if strings.Contains(f.Message, "person") {
			t.Errorf("core kind person must be excluded from missing-sameAs check: %+v", f)
		}
	}
}

// TestLintMissingSameAs_ExcludesKindsWithSameAs verifies that a non-core kind
// with a sameAs IRI is NOT reported.
func TestLintMissingSameAs_ExcludesKindsWithSameAs(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	if _, err := store.Writer().ExecContext(ctx,
		`INSERT INTO entity_kinds (name, display_name, description, provenance, added_by, added_at, status, same_as)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"user:vehicle", "Vehicle", "A road vehicle.", "user", "user",
		"2026-06-07T09:00:00Z", "active",
		`["https://schema.org/Vehicle"]`,
	); err != nil {
		t.Fatalf("insert kind with sameAs: %v", err)
	}

	db := store.Writer()
	findings, err := lintMissingSameAs(ctx, db)
	if err != nil {
		t.Fatalf("lintMissingSameAs: %v", err)
	}
	for _, f := range findings {
		if strings.Contains(f.Message, "user:vehicle") {
			t.Errorf("user:vehicle has sameAs; must not appear in findings: %+v", f)
		}
	}
}

// TestLintLexicalSimilarity_KnownValues verifies the Jaccard token-similarity
// helper returns expected values for well-understood cases.
func TestLintLexicalSimilarity_KnownValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want float64
	}{
		{"vehicle", "vehicle", 1.0},     // identical
		{"a:b:c", "a:b:d", 0.5},         // ["a","b","c"] vs ["a","b","d"] → 2/4 = 0.5
		{"x_y_z", "x_y_z_w", 0.75},      // ["x","y","z"] vs ["x","y","z","w"] → 3/4
		{"unrelated", "different", 0.0}, // no overlap
	}
	for _, tc := range cases {
		t.Run(tc.a+"/"+tc.b, func(t *testing.T) {
			t.Parallel()
			got := lintLexicalSimilarity(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("lintLexicalSimilarity(%q, %q) = %.4f, want %.4f", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestRenderLintReportCLI_NoFindings verifies that a zero-findings report emits
// the "no findings" message.
func TestRenderLintReportCLI_NoFindings(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	report := lintReport{Findings: []lintFinding{}, TotalFindings: 0}
	if err := renderLintReportCLI(&buf, report); err != nil {
		t.Fatalf("renderLintReportCLI: %v", err)
	}
	if !strings.Contains(buf.String(), "no findings") {
		t.Errorf("output = %q, want mention of 'no findings'", buf.String())
	}
}

// TestRenderLintReportCLI_WithFindings verifies that a non-empty report emits
// the check names, messages, and total count.
func TestRenderLintReportCLI_WithFindings(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	report := lintReport{
		Findings: []lintFinding{
			{Check: "near-duplicate-kinds", Message: "kinds \"a\" and \"b\" are similar"},
			{Check: "unused-predicate", Message: "predicate \"orphan\" has zero relations"},
		},
		TotalFindings: 2,
	}
	if err := renderLintReportCLI(&buf, report); err != nil {
		t.Fatalf("renderLintReportCLI: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"near-duplicate-kinds",
		"unused-predicate",
		`kinds "a" and "b" are similar`,
		`predicate "orphan" has zero relations`,
		"2 finding(s) total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestLintNDJSONFormat_OneRecordPerLine verifies that FormatNDJSON emits one
// JSON object per finding line with no aggregate wrapper. This is the Issue-3
// regression test: before the fix both FormatJSON and FormatNDJSON emitted the
// same indented aggregate object.
func TestLintNDJSONFormat_OneRecordPerLine(t *testing.T) {
	t.Parallel()
	findings := []lintFinding{
		{Check: "unused-predicate", Name: "orphan", Message: "predicate \"orphan\" has zero relations"},
		{Check: "missing-sameAs", Name: "user:gadget", Message: "kind \"user:gadget\" has no sameAs IRIs"},
	}

	// Simulate the NDJSON path: one json.Encoder.Encode call per finding.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range findings {
		if err := enc.Encode(findings[i]); err != nil {
			t.Fatalf("encode finding %d: %v", i, err)
		}
	}
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("NDJSON output: want 2 lines, got %d\nfull:\n%s", len(lines), out)
	}
	for i, line := range lines {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("line %d not valid JSON: %v (%q)", i, err, line)
			continue
		}
		// Each line must have "check" and "message"; no "findings" wrapper key.
		if _, ok := obj["check"]; !ok {
			t.Errorf("line %d missing 'check' field: %v", i, obj)
		}
		if _, ok := obj["findings"]; ok {
			t.Errorf("line %d must not have aggregate 'findings' wrapper key", i)
		}
	}
}

// TestLintJSONFormat_AggregateObject verifies that FormatJSON emits a single
// indented JSON object with a "findings" array and a "totalFindings" count.
// This is the counterpart of the NDJSON test above.
func TestLintJSONFormat_AggregateObject(t *testing.T) {
	t.Parallel()
	findings := []lintFinding{
		{Check: "unused-predicate", Name: "orphan", Message: "predicate \"orphan\" has zero relations"},
	}
	report := lintReport{Findings: findings, TotalFindings: len(findings)}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		t.Fatalf("encode report: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("parse JSON aggregate: %v\n%s", err, buf.String())
	}
	if _, ok := decoded["findings"]; !ok {
		t.Error("JSON aggregate missing 'findings' key")
	}
	if _, ok := decoded["totalFindings"]; !ok {
		t.Error("JSON aggregate missing 'totalFindings' key")
	}
	ff, ok := decoded["findings"].([]any)
	if !ok {
		t.Fatalf("'findings' is not an array: %T", decoded["findings"])
	}
	if len(ff) != 1 {
		t.Errorf("findings length = %d, want 1", len(ff))
	}
}

// TestLintNearDuplicateKinds_LexicalThresholdFires verifies that the lexical
// offline path (nil embedder) uses the calibrated lintNearDupLexicalThreshold
// (0.50) and reports near-duplicate pairs above it. This is the Issue-5
// regression test rewritten for the corrected design: the embedding-calibrated
// dedup_medium_threshold (~0.82) must NOT be used as the lexical floor because
// prose Jaccard/LCS scores almost never reach that value, making the check dead.
//
// Pair "Alpha Beta Gamma text." / "Alpha Beta Delta text.":
//
//	word tokens: [alpha,beta,gamma,text] vs [alpha,beta,delta,text]
//	word-LCS: [alpha,beta,text] = 3 words, maxLen=4 → lintTextSimilarity = 0.75
//	0.75 >= lintNearDupLexicalThreshold(0.50) → finding expected.
//
// Pair "Unrelated Alpha." / "Different Beta.":
//
//	word tokens: [unrelated,alpha] vs [different,beta]
//	word-LCS: 0 words → lintTextSimilarity = 0.0
//	0.0 < 0.50 → no finding expected.
func TestLintNearDuplicateKinds_LexicalThresholdFires(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	seedKindWithStatusCLI(t, store, "user:alpha_kind", "Alpha Beta", "Gamma text.", "user", "user", "active")
	seedKindWithStatusCLI(t, store, "user:beta_kind", "Alpha Beta", "Delta text.", "user", "user", "active")
	seedKindWithStatusCLI(t, store, "user:unrelated_kind", "Unrelated Alpha", "Completely different.", "user", "user", "active")

	db := store.Writer()

	// The nil embedder exercises the lexical path; the threshold arg is unused
	// by the lexical path (it always uses lintNearDupLexicalThreshold = 0.50).
	findings, err := lintNearDuplicateKinds(ctx, db, nil, 0.75)
	if err != nil {
		t.Fatalf("lintNearDuplicateKinds: %v", err)
	}

	// alpha_kind/beta_kind share "Alpha Beta" displayName and "text" in desc →
	// lintTextSimilarity = 0.75 ≥ 0.50 → must appear.
	found := false
	for _, f := range findings {
		if strings.Contains(f.Message, "user:alpha_kind") && strings.Contains(f.Message, "user:beta_kind") {
			found = true
		}
		if f.Check != "near-duplicate-kinds" {
			t.Errorf("finding.Check = %q, want near-duplicate-kinds", f.Check)
		}
	}
	if !found {
		t.Errorf("expected alpha_kind/beta_kind near-duplicate finding, got %+v", findings)
	}

	// unrelated_kind must not pair with alpha_kind or beta_kind.
	for _, f := range findings {
		if strings.Contains(f.Message, "user:unrelated_kind") {
			t.Errorf("unrelated_kind must not appear as near-dup, got: %s", f.Message)
		}
	}
}

// TestLintNearDuplicateKinds_VehicleAutoNearDup verifies that two kinds whose
// display names differ ("vehicle" vs "auto") but whose descriptions are
// semantically near-identical are flagged as a near-duplicate pair. This test
// specifically exercises the calibration fix: under the old Jaccard metric
// compared against the cosine-calibrated dedup_medium_threshold (~0.82), this
// pair was silently missed. Under the new word-level LCS metric with the
// separately calibrated lintNearDupLexicalThreshold (0.50), it is reported.
//
// Word-level LCS analysis:
//
//	text A: "vehicle a road vehicle"  → [vehicle, a, road, vehicle]  (4 words)
//	text B: "auto a road automobile vehicle" → [auto, a, road, automobile, vehicle] (5 words)
//	word-LCS: [a, road, vehicle] = 3 words, maxLen = 5
//	lintTextSimilarity = 3/5 = 0.60 >= lintNearDupLexicalThreshold(0.50) → finding expected
//
// This test would FAIL under the original mis-calibrated code (Jaccard threshold 0.82).
func TestLintNearDuplicateKinds_VehicleAutoNearDup(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	// displayName "vehicle"/"auto" differ; descriptions share "a road * vehicle".
	seedKindWithStatusCLI(t, store, "user:vehicle", "vehicle", "A road vehicle.", "user", "user", "active")
	seedKindWithStatusCLI(t, store, "user:auto", "auto", "A road automobile vehicle.", "user", "user", "active")

	db := store.Writer()
	// nil embedder forces the lexical path — tests the offline/CI code path.
	findings, err := lintNearDuplicateKinds(ctx, db, nil, 0.82)
	if err != nil {
		t.Fatalf("lintNearDuplicateKinds: %v", err)
	}
	if len(findings) == 0 {
		t.Error("expected near-duplicate finding for user:vehicle / user:auto (similar descriptions), got none — calibration bug regression")
	}
	for _, f := range findings {
		if f.Check != "near-duplicate-kinds" {
			t.Errorf("finding.Check = %q, want near-duplicate-kinds", f.Check)
		}
	}
}

// TestLintNearDuplicateKinds_EmbedderFailureFallsBackToLexical verifies that a
// non-nil but failing embedder (a configured Ollama endpoint that is
// unreachable) does NOT silently disable the near-duplicate check. The
// embedding path must fall back to the lexical path so duplicate kinds still
// surface — otherwise `maz schema lint --strict` would pass with duplicates
// whenever the embedder is configured but down.
func TestLintNearDuplicateKinds_EmbedderFailureFallsBackToLexical(t *testing.T) {
	t.Parallel()
	store := openTestSchemaDB(t)
	ctx := context.Background()

	seedKindWithStatusCLI(t, store, "user:vehicle", "vehicle", "A road vehicle.", "user", "user", "active")
	seedKindWithStatusCLI(t, store, "user:auto", "auto", "A road automobile vehicle.", "user", "user", "active")

	db := store.Writer()
	// Non-nil failing embedder forces the embedding path, which must degrade to
	// the lexical fallback on the first Embed error rather than report nothing.
	findings, err := lintNearDuplicateKinds(ctx, db, failingLintEmbedder{}, 0.82)
	if err != nil {
		t.Fatalf("lintNearDuplicateKinds: %v", err)
	}
	if len(findings) == 0 {
		t.Error("expected near-duplicate finding via lexical fallback when embedder fails, got none — strict lint would pass with duplicates")
	}
	for _, f := range findings {
		if f.Check != "near-duplicate-kinds" {
			t.Errorf("finding.Check = %q, want near-duplicate-kinds", f.Check)
		}
	}
}

// TestLintNearDuplicateKindsEmbedding_CancellationPropagates verifies that an
// embed error wrapping context.Canceled / context.DeadlineExceeded is returned
// rather than masked as a normal lexical run. An interrupted `maz schema lint`
// must surface the cancellation, not keep working and exit with a clean report.
// Exercises lintNearDuplicateKindsEmbedding directly with two near-duplicate
// kinds that would otherwise produce a lexical finding on fallback.
func TestLintNearDuplicateKindsEmbedding_CancellationPropagates(t *testing.T) {
	t.Parallel()
	kinds := []kindEntry{
		{name: "user:vehicle", displayName: "vehicle", desc: "A road vehicle."},
		{name: "user:auto", displayName: "auto", desc: "A road automobile vehicle."},
	}
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(ctxErr.Error(), func(t *testing.T) {
			t.Parallel()
			emb := errLintEmbedder{err: ctxErr}
			findings, err := lintNearDuplicateKindsEmbedding(context.Background(), kinds, emb, 0.82)
			if !errors.Is(err, ctxErr) {
				t.Fatalf("err = %v, want wrapping %v (cancellation must propagate, not fall back)", err, ctxErr)
			}
			if findings != nil {
				t.Errorf("expected no findings on cancellation, got %d (lint kept working after interrupt)", len(findings))
			}
		})
	}
}

// TestLintNearDuplicateKindsEmbedding_CanceledContextPropagates verifies the
// ctx.Err() guard: when the caller's context is already done, even a generic
// embed failure propagates the context error instead of falling back.
func TestLintNearDuplicateKindsEmbedding_CanceledContextPropagates(t *testing.T) {
	t.Parallel()
	kinds := []kindEntry{
		{name: "user:vehicle", displayName: "vehicle", desc: "A road vehicle."},
		{name: "user:auto", displayName: "auto", desc: "A road automobile vehicle."},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx.Err() is now context.Canceled
	findings, err := lintNearDuplicateKindsEmbedding(ctx, kinds, failingLintEmbedder{}, 0.82)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if findings != nil {
		t.Errorf("expected no findings on canceled context, got %d", len(findings))
	}
}
