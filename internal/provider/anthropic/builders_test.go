package anthropic

import (
	"encoding/json"
	"testing"
)

func TestBuildCachedSystemPrompt(t *testing.T) {
	tests := []struct {
		name         string
		parts        []SystemPart
		wantLen      int
		wantMarkerAt int // -1 means no block should carry CacheControl
		wantTexts    []string
	}{
		{
			name:         "empty input returns nil",
			parts:        nil,
			wantLen:      0,
			wantMarkerAt: -1,
		},
		{
			name:         "single static part marks block 0",
			parts:        []SystemPart{{Text: "sys", Static: true}},
			wantLen:      1,
			wantMarkerAt: 0,
			wantTexts:    []string{"sys"},
		},
		{
			name: "three static parts mark last block only",
			parts: []SystemPart{
				{Text: "a", Static: true},
				{Text: "b", Static: true},
				{Text: "c", Static: true},
			},
			wantLen:      3,
			wantMarkerAt: 2,
			wantTexts:    []string{"a", "b", "c"},
		},
		{
			name: "mixed static then dynamic marks last static",
			parts: []SystemPart{
				{Text: "static-1", Static: true},
				{Text: "static-2", Static: true},
				{Text: "dynamic-tail", Static: false},
			},
			wantLen:      3,
			wantMarkerAt: 1,
			wantTexts:    []string{"static-1", "static-2", "dynamic-tail"},
		},
		{
			name: "all dynamic carries no marker",
			parts: []SystemPart{
				{Text: "d1", Static: false},
				{Text: "d2", Static: false},
			},
			wantLen:      2,
			wantMarkerAt: -1,
			wantTexts:    []string{"d1", "d2"},
		},
		{
			// A dynamic part BEFORE a static one means there is no contiguous
			// static prefix. Marking the trailing static block would bake the
			// preceding dynamic text into the cached prefix and break the
			// per-call cache hit. The marker must NOT land on the trailing
			// static block.
			name: "leading dynamic disqualifies trailing static from marker",
			parts: []SystemPart{
				{Text: "d-head", Static: false},
				{Text: "s-tail", Static: true},
			},
			wantLen:      2,
			wantMarkerAt: -1,
			wantTexts:    []string{"d-head", "s-tail"},
		},
		{
			// The static prefix ends at the first dynamic block; any static
			// block that appears after the first dynamic block is outside the
			// cache-eligible prefix and must not receive the marker.
			name: "static prefix ends at first dynamic, later static is not marked",
			parts: []SystemPart{
				{Text: "s0", Static: true},
				{Text: "d1", Static: false},
				{Text: "s2", Static: true},
			},
			wantLen:      3,
			wantMarkerAt: 0,
			wantTexts:    []string{"s0", "d1", "s2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildCachedSystemPrompt(tt.parts)

			if tt.parts == nil {
				if got != nil {
					t.Errorf("empty input: got %v, want nil", got)
				}
				return
			}

			if len(got) != tt.wantLen {
				t.Fatalf("len: got %d, want %d", len(got), tt.wantLen)
			}

			for i, b := range got {
				if b.Type != ContentTypeText {
					t.Errorf("block[%d].Type: got %q, want %q", i, b.Type, ContentTypeText)
				}
				if b.Text != tt.wantTexts[i] {
					t.Errorf("block[%d].Text: got %q, want %q", i, b.Text, tt.wantTexts[i])
				}
				switch i {
				case tt.wantMarkerAt:
					if b.CacheControl == nil {
						t.Errorf("block[%d]: expected CacheControl marker, got nil", i)
					} else if b.CacheControl.Type != "ephemeral" {
						t.Errorf("block[%d].CacheControl.Type: got %q, want %q", i, b.CacheControl.Type, "ephemeral")
					}
				default:
					if b.CacheControl != nil {
						t.Errorf("block[%d]: unexpected CacheControl %+v", i, b.CacheControl)
					}
				}
			}
		})
	}
}

func TestBuildCachedSystemPromptJSONShape(t *testing.T) {
	got := BuildCachedSystemPrompt([]SystemPart{
		{Text: "sys-a", Static: true},
		{Text: "sys-b", Static: true},
	})

	buf, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	want := `[{"type":"text","text":"sys-a"},{"type":"text","text":"sys-b","cache_control":{"type":"ephemeral"}}]`
	if string(buf) != want {
		t.Errorf("wire JSON mismatch\n got: %s\nwant: %s", buf, want)
	}
}

func TestBuildCachedSystemPromptEmptyOmits(t *testing.T) {
	req := &CompleteRequest{
		Model:     "claude-opus-4-7",
		MaxTokens: 16,
		System:    BuildCachedSystemPrompt(nil),
		Messages:  BuildMessages(nil, "hi"),
	}

	buf, err := json.Marshal(toWire(req))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(buf); contains(got, `"system"`) {
		t.Errorf("expected system field to be omitted when nil; got %s", got)
	}
}

func TestBuildMessages(t *testing.T) {
	t.Run("empty history yields single user message", func(t *testing.T) {
		got := BuildMessages(nil, "hello")
		if len(got) != 1 {
			t.Fatalf("len: got %d, want 1", len(got))
		}
		if got[0].Role != RoleUser {
			t.Errorf("role: got %q, want %q", got[0].Role, RoleUser)
		}
		if len(got[0].Content) != 1 || got[0].Content[0].Text != "hello" {
			t.Errorf("content: got %+v, want single text 'hello'", got[0].Content)
		}
	})

	t.Run("history preserved verbatim with new user appended", func(t *testing.T) {
		history := []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("q1")}},
			{Role: RoleAssistant, Content: []ContentBlock{TextBlock("a1")}},
		}
		got := BuildMessages(history, "q2")

		if len(got) != 3 {
			t.Fatalf("len: got %d, want 3", len(got))
		}
		if got[0].Role != RoleUser || got[0].Content[0].Text != "q1" {
			t.Errorf("got[0]: %+v, want {user, q1}", got[0])
		}
		if got[1].Role != RoleAssistant || got[1].Content[0].Text != "a1" {
			t.Errorf("got[1]: %+v, want {assistant, a1}", got[1])
		}
		if got[2].Role != RoleUser || got[2].Content[0].Text != "q2" {
			t.Errorf("got[2]: %+v, want {user, q2}", got[2])
		}
	})

	t.Run("input slice not mutated by callers of the result", func(t *testing.T) {
		history := []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("orig-0")}},
			{Role: RoleAssistant, Content: []ContentBlock{TextBlock("orig-1")}},
		}
		got := BuildMessages(history, "new")

		// Overwrite the returned slice's first element. The input slice's
		// first element must not change, proving the helper allocated a
		// fresh backing array.
		got[0] = Message{Role: RoleAssistant, Content: []ContentBlock{TextBlock("clobbered")}}

		if history[0].Role != RoleUser || history[0].Content[0].Text != "orig-0" {
			t.Errorf("input mutated: history[0] = %+v", history[0])
		}
		if history[1].Role != RoleAssistant || history[1].Content[0].Text != "orig-1" {
			t.Errorf("input mutated: history[1] = %+v", history[1])
		}
		if len(history) != 2 {
			t.Errorf("input len changed: %d", len(history))
		}
	})

	t.Run("nested content slice not aliased with input", func(t *testing.T) {
		// A shallow outer-slice copy still leaves Message.Content sharing
		// its backing array with the input. Mutating got[i].Content[j]
		// would then reach back into history[i].Content[j] and silently
		// destabilize the cache-eligible prefix on subsequent calls.
		history := []Message{
			{Role: RoleUser, Content: []ContentBlock{TextBlock("orig-text")}},
		}
		got := BuildMessages(history, "new")

		got[0].Content[0] = TextBlock("clobbered")

		if history[0].Content[0].Text != "orig-text" {
			t.Errorf("nested mutation leaked into input: history[0].Content[0].Text = %q, want %q",
				history[0].Content[0].Text, "orig-text")
		}
	})
}

func TestBuildMessagesJSONShape(t *testing.T) {
	history := []Message{
		{Role: RoleUser, Content: []ContentBlock{TextBlock("prev")}},
	}
	got := BuildMessages(history, "now")

	buf, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"role":"user","content":[{"type":"text","text":"prev"}]},{"role":"user","content":[{"type":"text","text":"now"}]}]`
	if string(buf) != want {
		t.Errorf("wire JSON mismatch\n got: %s\nwant: %s", buf, want)
	}
}

// contains reports whether substr is in s. Inlined here to avoid the
// strings import in this test file's narrow needs.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
