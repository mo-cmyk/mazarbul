package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// TestGetSourceRef_HappyPath asserts a found SourceRef is returned with its
// fields and the unverified trust label.
func TestGetSourceRef_HappyPath(t *testing.T) {
	t.Parallel()
	g := NewGetSourceRef(&fakeNavigator{
		sourceRef: &navigate.SourceRef{
			UID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Name: "sourceRefs/01ARZ3NDEKTSV4RRFFQ69G5FAV", SubjectUID: "subj1",
			SourceType: "email", ExternalURL: "https://mail.example.com/msg-42",
			ContentHash: "abc", FetchTime: "2026-06-15T10:00:00Z", DedupKey: "gmail:msg-42",
			SourceEventUID: "ev1",
		},
	})
	req, _ := json.Marshal(map[string]any{"name": "sourceRefs/01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	out, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var resp struct {
		Name        string `json:"name"`
		UID         string `json:"uid"`
		SourceType  string `json:"sourceType"`
		ExternalURL string `json:"externalUrl"`
		FetchTime   string `json:"fetchTime"`
		ContentHash string `json:"contentHash"`
		TrustState  string `json:"trustState"`
	}
	if uerr := json.Unmarshal(out, &resp); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if resp.TrustState != "unverified" {
		t.Errorf("trustState = %q, want unverified", resp.TrustState)
	}
	if resp.Name != "sourceRefs/01ARZ3NDEKTSV4RRFFQ69G5FAV" || resp.SourceType != "email" {
		t.Errorf("response = %+v", resp)
	}
	if resp.ExternalURL != "https://mail.example.com/msg-42" || resp.FetchTime != "2026-06-15T10:00:00Z" || resp.ContentHash != "abc" {
		t.Errorf("response missing expected fields: %+v", resp)
	}
}

// TestGetSourceRef_NotFound asserts a well-formed name whose row is absent maps
// to E_NOT_FOUND.
func TestGetSourceRef_NotFound(t *testing.T) {
	t.Parallel()
	g := NewGetSourceRef(&fakeNavigator{}) // sourceRef nil → ErrNotFound
	req, _ := json.Marshal(map[string]any{"name": "sourceRefs/01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	_, err := g.Invoke(context.Background(), req)
	wantCode(t, err, mcp.CodeNotFound)
}

// TestGetSourceRef_ValidationErrors asserts malformed names are E_VALIDATION
// (not E_NOT_FOUND) and fail before any navigator call: a bare uid, a wrong
// collection, an empty uid, and a uid that is not a structurally-valid ULID.
func TestGetSourceRef_ValidationErrors(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"01ARZ3NDEKTSV4RRFFQ69G5FAV",        // bare uid, no prefix
		"facts/01ARZ3NDEKTSV4RRFFQ69G5FAV",  // wrong collection
		"sourceRefs/",                       // empty uid
		"sourceRefs/not-a-valid-ulid",       // uid is not a ULID (bad charset + length)
		"sourceRefs/01ARZ3NDEKTSV4RRFFQ69G", // uid too short (23 chars)
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := NewGetSourceRef(&fakeNavigator{})
			req, _ := json.Marshal(map[string]any{"name": name})
			_, err := g.Invoke(context.Background(), req)
			wantCode(t, err, mcp.CodeValidation)
		})
	}
}

// TestGetSourceRef_BadJSON asserts an undecodable request body is E_VALIDATION.
func TestGetSourceRef_BadJSON(t *testing.T) {
	t.Parallel()
	g := NewGetSourceRef(&fakeNavigator{})
	_, err := g.Invoke(context.Background(), json.RawMessage(`{"name":`))
	wantCode(t, err, mcp.CodeValidation)
}

// roundtripAppender captures appended events so the record→get roundtrip can
// reconstruct the projection a real dispatcher would build.
type roundtripAppender struct{ events []kernel.Event }

func (a *roundtripAppender) Append(_ context.Context, evs []kernel.Event) ([]kernel.Receipt, error) {
	a.events = append(a.events, evs...)
	return []kernel.Receipt{{UID: "ev1"}}, nil
}

// TestSourceRef_RecordThenGet_Roundtrip exercises the full write→read contract:
// recordSourceRef appends an event and returns a resource name; getSourceRef
// invoked with THAT name surfaces the recorded fields with the unverified label.
// It also asserts the returned name's uid is the dedupKey-derived identity — the
// property that makes the name resolvable against the real projection. A fake
// navigator seeded from the recorded payload stands in for that projection.
func TestSourceRef_RecordThenGet_Roundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const dedupKey = "slack:c1:ts1"
	app := &roundtripAppender{}
	recOut, err := NewRecordSourceRef(app).Invoke(ctx, json.RawMessage(
		`{"subjectUid":"01ARZ3NDEKTSV4RRFFQ69G5FB0","sourceType":"slack_message","dedupKey":"`+dedupKey+`","connector":"slack","externalUrl":"https://slack.example.com/c1/ts1"}`))
	if err != nil {
		t.Fatalf("record Invoke: %v", err)
	}
	var recResp struct {
		Name string `json:"name"`
	}
	if uerr := json.Unmarshal(recOut, &recResp); uerr != nil {
		t.Fatalf("decode record response: %v", uerr)
	}
	if want := "sourceRefs/" + string(ontology.SourceRefUID(dedupKey)); recResp.Name != want {
		t.Errorf("recorded name = %q, want dedupKey-derived %q", recResp.Name, want)
	}

	var pl ontology.SourceRefRecordedEvent
	if uerr := json.Unmarshal(app.events[0].Payload, &pl); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	g := NewGetSourceRef(&fakeNavigator{sourceRef: &navigate.SourceRef{
		UID: string(pl.UID), Name: "sourceRefs/" + string(pl.UID),
		SubjectUID: string(pl.SubjectUID), SourceType: pl.SourceType,
		Connector: pl.Connector, ExternalURL: pl.ExternalURL,
		FetchTime: pl.FetchTime.UTC().Format(time.RFC3339Nano), DedupKey: pl.DedupKey,
		SourceEventUID: app.events[0].UID,
	}})

	getReq, _ := json.Marshal(map[string]any{"name": recResp.Name})
	getOut, err := g.Invoke(ctx, getReq)
	if err != nil {
		t.Fatalf("get Invoke with recorded name %q: %v", recResp.Name, err)
	}
	var got struct {
		Name        string `json:"name"`
		SourceType  string `json:"sourceType"`
		ExternalURL string `json:"externalUrl"`
		TrustState  string `json:"trustState"`
	}
	if uerr := json.Unmarshal(getOut, &got); uerr != nil {
		t.Fatalf("decode get response: %v", uerr)
	}
	if got.SourceType != "slack_message" || got.ExternalURL != "https://slack.example.com/c1/ts1" {
		t.Errorf("recorded fields did not survive write→read: %+v", got)
	}
	if got.TrustState != "unverified" {
		t.Errorf("trustState = %q, want unverified", got.TrustState)
	}
}
