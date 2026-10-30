package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/ingest"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

type fakeLister struct {
	rows []ingest.WatermarkStatus
	err  error
}

func (f fakeLister) ListWatermarks(context.Context) ([]ingest.WatermarkStatus, error) {
	return f.rows, f.err
}

func TestListSources_ReturnsConnectorsAndWatermarks(t *testing.T) {
	t.Parallel()
	l := NewListSources(fakeLister{rows: []ingest.WatermarkStatus{
		{Connector: "local-git", Stream: "commits", Sequence: 12, AdvanceTime: "2026-06-17T12:00:00Z"},
	}})
	out, err := l.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var got listSourcesOutput
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	foundGit, foundChats := false, false
	for _, c := range got.Connectors {
		switch c {
		case "local-git":
			foundGit = true
		case "claude-chats":
			foundChats = true
		}
	}
	if !foundGit || !foundChats {
		t.Errorf("connectors = %v, want to include local-git and claude-chats", got.Connectors)
	}
	if len(got.Watermarks) != 1 || got.Watermarks[0].Sequence != 12 {
		t.Errorf("watermarks = %+v, want one row seq 12", got.Watermarks)
	}
}

func TestListSources_EmptyWatermarksIsEmptyArray(t *testing.T) {
	t.Parallel()
	l := NewListSources(fakeLister{rows: nil})
	out, err := l.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// watermarks must serialize as [] not null for a stable agent contract.
	if !json.Valid(out) {
		t.Fatal("invalid JSON")
	}
	var raw map[string]json.RawMessage
	if uerr := json.Unmarshal(out, &raw); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if string(raw["watermarks"]) != "[]" {
		t.Errorf("watermarks = %s, want []", raw["watermarks"])
	}
}

func TestListSources_ListerErrorIsInternal(t *testing.T) {
	t.Parallel()
	l := NewListSources(fakeLister{err: errors.New("db gone")})
	_, err := l.Invoke(context.Background(), nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var te *mcp.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("err type = %T, want *mcp.ToolError", err)
	}
	if te.Code != mcp.CodeInternal {
		t.Errorf("err.Code = %q, want %q", te.Code, mcp.CodeInternal)
	}
}
