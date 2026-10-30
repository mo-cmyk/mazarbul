package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func revs(uids ...string) []docRevisionRow {
	out := make([]docRevisionRow, len(uids))
	for i, u := range uids {
		out[i] = docRevisionRow{UID: u, Kind: "revision", Body: u + " body"}
	}
	return out
}

func TestContentRevisions_DropsTombstones(t *testing.T) {
	t.Parallel()
	rows := []docRevisionRow{
		{UID: "A", Kind: "revision"},
		{UID: "B", Kind: "deleted"},
		{UID: "C", Kind: "revision"},
	}
	got := contentRevisions(rows)
	if len(got) != 2 || got[0].UID != "A" || got[1].UID != "C" {
		t.Fatalf("contentRevisions = %+v, want [A C]", got)
	}
}

func TestPickDiffRevisions_DefaultsToLastTwo(t *testing.T) {
	t.Parallel()
	a, b, err := pickDiffRevisions(revs("R1", "R2", "R3"), nil)
	if err != nil {
		t.Fatalf("pickDiffRevisions: %v", err)
	}
	if a.UID != "R2" || b.UID != "R3" {
		t.Errorf("default pair = (%s,%s), want (R2,R3)", a.UID, b.UID)
	}
}

func TestPickDiffRevisions_OneArgDiffsAgainstLatest(t *testing.T) {
	t.Parallel()
	a, b, err := pickDiffRevisions(revs("R1", "R2", "R3"), []string{"R1"})
	if err != nil {
		t.Fatalf("pickDiffRevisions: %v", err)
	}
	if a.UID != "R1" || b.UID != "R3" {
		t.Errorf("one-arg pair = (%s,%s), want (R1,R3)", a.UID, b.UID)
	}
}

func TestPickDiffRevisions_TwoExplicitArgs(t *testing.T) {
	t.Parallel()
	a, b, err := pickDiffRevisions(revs("R1", "R2", "R3"), []string{"R1", "R2"})
	if err != nil {
		t.Fatalf("pickDiffRevisions: %v", err)
	}
	if a.UID != "R1" || b.UID != "R2" {
		t.Errorf("two-arg pair = (%s,%s), want (R1,R2)", a.UID, b.UID)
	}
}

func TestMatchRevision_PrefixAndErrors(t *testing.T) {
	t.Parallel()
	rs := revs("01ABCDEF", "01ABZZZZ", "01XYWWWW")

	// Unambiguous prefix.
	got, err := matchRevision(rs, "01xyw") // lowercase tolerated
	if err != nil {
		t.Fatalf("matchRevision: %v", err)
	}
	if got.UID != "01XYWWWW" {
		t.Errorf("matched %q, want 01XYWWWW", got.UID)
	}

	// Ambiguous prefix.
	if _, err := matchRevision(rs, "01AB"); err == nil {
		t.Error("expected ambiguity error for prefix 01AB")
	}

	// No match.
	if _, err := matchRevision(rs, "ZZZ"); err == nil {
		t.Error("expected no-match error for prefix ZZZ")
	}
}

func TestRenderDocDiff_RefusesTruncatedBody(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)

	resp := docHistoryResponse{
		Path: "notes/big.md",
		Revisions: []docRevisionRow{
			{UID: "R1", Kind: "revision", Body: "small"},
			{UID: "R2", Kind: "revision", Body: "partial", BodyTruncated: true},
		},
	}
	err := renderDocDiff(cmd, resp, nil)
	if err == nil {
		t.Fatalf("renderDocDiff over a truncated body returned nil; want a clear refusal")
	}
	// The refusal must point the user at the verbatim vault file rather than
	// printing a (wrong) diff over partial content. A buffered (non-TTY) stdout
	// resolves to JSON mode, so the only thing on stdout is the structured
	// {error,code} refusal envelope — never diff hunks (+/- lines).
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
			t.Errorf("a diff hunk was printed despite truncation: %q", out.String())
			break
		}
	}
	if out.Len() > 0 && !strings.Contains(out.String(), "too large to diff") {
		t.Errorf("stdout = %q, want the structured refusal envelope", out.String())
	}
	if !strings.Contains(errBuf.String(), "notes/big.md") || !strings.Contains(errBuf.String(), "too large to diff") {
		t.Errorf("refusal message = %q, want it to name the vault file and explain", errBuf.String())
	}
}
