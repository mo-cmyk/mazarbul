package cli

// helptopics_test.go pins the rendering contract the user hit: `maz help-topics
// <topic>` must glamour-render on a styled TTY (the user saw raw markdown
// because the renderer was a no-op stub), yet stay raw markdown when styling is
// off so `| less` and coding agents get clean, parseable text.

import (
	"bytes"
	"strings"
	"testing"
)

func runHelpTopic(t *testing.T, topic string) string {
	t.Helper()
	cmd := newHelpTopicsCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{topic})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("help-topics %s: %v", topic, err)
	}
	return buf.String()
}

func TestHelpTopicsListIncludesConcepts(t *testing.T) {
	// concepts must appear in the topic list so `maz help-topics` shows it.
	t.Setenv("NO_COLOR", "1")
	cmd := newHelpTopicsCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("help-topics list: %v", err)
	}
	if !strings.Contains(buf.String(), "concepts") {
		t.Errorf("help-topics list must contain 'concepts'; got:\n%s", buf.String())
	}
}

func TestHelpTopicConceptsRendersRaw(t *testing.T) {
	// Verify the concepts topic is embedded and returns the expected heading.
	t.Setenv("NO_COLOR", "1")
	out := runHelpTopic(t, "concepts")
	if !strings.Contains(out, "# Concepts") {
		t.Errorf("concepts topic must start with `# Concepts`; got:\n%s", out)
	}
	if !strings.Contains(out, "Episode") {
		t.Errorf("concepts topic must describe the Episode data type; got:\n%s", out)
	}
}

func TestHelpTopicShow_RawWhenStyleOff(t *testing.T) {
	// NO_COLOR wins unconditionally → raw markdown, verbatim heading preserved.
	t.Setenv("NO_COLOR", "1")
	out := runHelpTopic(t, "recall")
	if !strings.Contains(out, "# Recall") {
		t.Errorf("NO_COLOR output should be raw markdown with the verbatim `# Recall` heading; got:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR output must not contain ANSI escapes; got:\n%q", out)
	}
}

func TestHelpTopicShow_StyledWhenForced(t *testing.T) {
	// CLICOLOR_FORCE forces styling on in the default (auto) mode even though the
	// test writer is not a real TTY — proves the glamour path is actually wired
	// (the old no-op stub would emit raw markdown here too).
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	out := runHelpTopic(t, "recall")
	if !strings.Contains(out, "\x1b[") {
		t.Errorf("forced-style output should be glamour-rendered with ANSI escapes; got:\n%q", out)
	}
	if strings.Contains(out, "# Recall") {
		t.Errorf("forced-style output should transform the markdown heading, not pass `# Recall` through; got:\n%s", out)
	}
}
