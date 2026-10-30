package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// recallHitsWithTrust invokes recall over a single hit carrying eventSource and
// returns the decoded hits. compact toggles the compact envelope.
func recallHitsWithTrust(t *testing.T, eventSource string, compact bool) []map[string]any {
	t.Helper()
	fake := &fakeQuery{yields: []fakeYield{
		{hit: kernel.Hit{
			UID:         "01HX-TRUST-000000000000001",
			Kind:        "remember",
			Text:        "Dana said the Zephyr launch slips to Q3",
			Confidence:  1.0,
			CreateTime:  time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			FusedScore:  0.5,
			EventSource: eventSource,
		}},
	}}
	r := tools.NewRecall(fake)
	req := `{"query":"zephyr"}`
	if compact {
		req = `{"query":"zephyr","compact":true}`
	}
	resp, err := r.Invoke(context.Background(), json.RawMessage(req))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got struct {
		Hits []map[string]any `json:"hits"`
	}
	if uerr := json.Unmarshal(resp, &got); uerr != nil {
		t.Fatalf("unmarshal response: %v", uerr)
	}
	if len(got.Hits) != 1 {
		t.Fatalf("hits len = %d, want 1", len(got.Hits))
	}
	return got.Hits
}

// TestRecall_UntrustedSourceCarriesTrustLabel pins the #212 recall-consumer
// protection: a hit from a connector source carries a well-explained trust
// label (state=proposed, the source tag, a note pointing at promoteRelation), in
// both verbose and compact envelopes.
func TestRecall_UntrustedSourceCarriesTrustLabel(t *testing.T) {
	t.Parallel()
	for _, compact := range []bool{false, true} {
		hits := recallHitsWithTrust(t, "connector:granola", compact)
		trust, ok := hits[0]["trust"].(map[string]any)
		if !ok {
			t.Fatalf("compact=%v: hits[0].trust missing or not an object; hit = %v", compact, hits[0])
		}
		if trust["state"] != "proposed" {
			t.Errorf("compact=%v: trust.state = %v, want proposed", compact, trust["state"])
		}
		if trust["source"] != "connector:granola" {
			t.Errorf("compact=%v: trust.source = %v, want connector:granola", compact, trust["source"])
		}
		note, _ := trust["note"].(string)
		if note == "" {
			t.Errorf("compact=%v: trust.note is empty; the label must explain the posture", compact)
		}
	}
}

// TestRecall_TrustedSourceHasNoLabel confirms a normal (non-connector) hit
// carries no trust key, so the label only appears where it matters and trusted
// recall stays lean.
func TestRecall_TrustedSourceHasNoLabel(t *testing.T) {
	t.Parallel()
	for _, src := range []string{"cli:remember", "extractor/v1", ""} {
		hits := recallHitsWithTrust(t, src, false)
		if _, present := hits[0]["trust"]; present {
			t.Errorf("source %q: hits[0].trust present, want absent for a trusted origin", src)
		}
	}
}
