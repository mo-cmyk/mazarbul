package ingest

import (
	"strings"
	"testing"
)

func TestScrubber_RedactsSecretShapes(t *testing.T) {
	t.Parallel()
	s := NewScrubber()
	cases := []struct {
		name   string
		secret string
	}{
		{"github classic token", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"github fine-grained pat", "github_pat_11ABCDEFG0abcdefghijkl_mnopqrstuvwxyz1234567890ABCD"},
		{"openai key", "sk-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"anthropic key", "sk-ant-abcdefghijklmnopqrstuvwxyz0123"},
		{"aws access key id", "AKIAIOSFODNN7EXAMPLE"},
		{"google api key", "AIzaSyA1234567890abcdefghijklmnopqrstuv"},
		{"slack bot token", "xoxb-1234567890-abcdefghijkl"},
		{"key value assignment", `password: "hunter2hunter2hunter2"`},
		{"granola api key", "grn_abcdefghijklmnopqrstuvwxyz0123"},
		// Real Granola production format is grn_<env>_<random> (underscore
		// sub-segments). The short "live"/"test" env segment must not stop the
		// match — these shapes leaked entirely (live) or partially (long prefix
		// then "_tail") under a [A-Za-z0-9]-only body. Found via live ingest.
		{"granola live env key", "grn_live_0fFa12kAeKfE3aFkA4"},
		{"granola test env key", "grn_test_0fFa12kAeKfE3a4"},
		{"granola long segmented key", "grn_01Ffa2kAeKfakEF3AKE45eF6_fAKakefEaFAK7kefEak"},
		{"bearer opaque token", "Bearer abcdefghijklmnopqrstuvwxyz0123456789"},
		{"bearer grn key", "Bearer grn_abcdefghijklmnopqrstuvwxyz0123"},
		{"padded base64 secret", "aGVsbG8gd29ybGQgdGhpcyBpcyBhIHNlY3JldA=="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := "context before " + tc.secret + " context after"
			out := s.Scrub(in)
			if strings.Contains(out, tc.secret) {
				t.Fatalf("secret survived scrub: %q -> %q", in, out)
			}
			if !strings.Contains(out, secretRedaction) {
				t.Errorf("no redaction marker in %q", out)
			}
		})
	}
}

// TestScrubber_RedactsConnectionStringCredentials pins the URL-userinfo leak the
// red-team found: a password embedded in a connection string
// (scheme://user:pass@host) must not survive into the stored episode.
func TestScrubber_RedactsConnectionStringCredentials(t *testing.T) {
	t.Parallel()
	s := NewScrubber()
	cases := []struct{ name, line, leak string }{
		{"postgres url", "DATABASE_URL=postgres://admin:SuperSecretPassword123@prod-db.example.com:5432/app", "SuperSecretPassword123"},
		{"redis url empty user", "REDIS_URL=redis://:anotherpassword456@redis.example.com:6379/0", "anotherpassword456"},
		{"mongodb srv uri", "uri = mongodb+srv://dbuser:Pa55wordValue@cluster0.example.com/db", "Pa55wordValue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := s.Scrub(tc.line)
			if strings.Contains(out, tc.leak) {
				t.Errorf("connection-string password survived scrub: %q -> %q", tc.line, out)
			}
			if !strings.Contains(out, secretRedaction) {
				t.Errorf("no redaction marker in %q", out)
			}
		})
	}
}

// TestScrubber_RedactsAWSKeyWithTrailingChars pins that an AWS key id glued to
// trailing characters is fully redacted (no residue after [REDACTED]).
func TestScrubber_RedactsAWSKeyWithTrailingChars(t *testing.T) {
	t.Parallel()
	s := NewScrubber()
	out := s.Scrub("creds AKIAIOSFODNN7EXAMPLE1234 end")
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("aws key survived: %q", out)
	}
	if strings.Contains(out, "1234") {
		t.Errorf("aws key trailing residue survived: %q", out)
	}
	if !strings.Contains(out, "creds") || !strings.Contains(out, "end") {
		t.Errorf("surrounding context lost: %q", out)
	}
}

func TestScrubber_LeavesCleanTextUntouched(t *testing.T) {
	t.Parallel()
	s := NewScrubber()
	clean := "Refactor the resolver to thread trust through processCandidate.\n\nFixes #211."
	if got := s.Scrub(clean); got != clean {
		t.Errorf("clean text mutated:\n  in:  %q\n  out: %q", clean, got)
	}
}

func TestScrubber_IsIdempotent(t *testing.T) {
	t.Parallel()
	s := NewScrubber()
	once := s.Scrub("token=abcdefghijklmnopqrstu and ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	twice := s.Scrub(once)
	if once != twice {
		t.Errorf("scrub not idempotent:\n  once:  %q\n  twice: %q", once, twice)
	}
}
