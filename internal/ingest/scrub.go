package ingest

import "regexp"

// secretRedaction is the placeholder substituted for any secret-shaped span.
const secretRedaction = "[REDACTED]"

// secretPatterns matches common credential shapes. The scrubber runs over
// untrusted external bodies (commit messages, PR prose) BEFORE they reach the
// event log, so a secret a contributor accidentally pasted into a commit is
// never durably stored in cleartext. The patterns are intentionally specific
// (known token prefixes + structured shapes) to keep false positives low; the
// goal is to catch the obvious leaks, not to be a perfect DLP engine.
//
//nolint:gochecknoglobals // process-lifetime compiled regex table, never mutated
var secretPatterns = []*regexp.Regexp{
	// GitHub tokens: ghp_, gho_, ghu_, ghs_, ghr_, github_pat_.
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
	// OpenAI / Anthropic style keys: sk-..., sk-ant-....
	regexp.MustCompile(`sk-(?:ant-)?[A-Za-z0-9_-]{20,}`),
	// Granola first-party API keys. The real production format is
	// grn_<env>_<random> (env = live/test/dev), so the body MUST allow the
	// underscore sub-segments — a charset of [A-Za-z0-9] alone stops at the first
	// "_" and leaks the rest (or the whole key when the env segment is short, as
	// in grn_live_...). Underscore is in the class and the threshold is 16 so the
	// segmented shapes are fully redacted.
	regexp.MustCompile(`grn_[A-Za-z0-9_]{16,}`),
	// Bearer authorization values (OAuth access tokens, opaque API keys). The
	// token charset is the union of base64url, JWT, and opaque-key alphabets.
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	// AWS access key id. {16,} (not {16}) so a key glued to trailing
	// characters (AKIA...EXAMPLE1234) is fully redacted, leaving no residue.
	regexp.MustCompile(`AKIA[0-9A-Z]{16,}`),
	// Google API key.
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	// Slack tokens: xoxb-, xoxp-, xoxa-, xoxr-.
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// JWT (three base64url segments).
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	// PEM private key blocks.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
	// Padded base64 blobs (encoded keys / binary secrets). Padding is required
	// so we redact genuine base64 secrets without nuking 40-char git SHAs or
	// other unpadded hex/ID runs that share the base64 charset. Residual gap:
	// an UNPADDED base64/base64url token with no known prefix is indistinguishable
	// from an opaque ID/hash, so it is deliberately not matched here — the
	// prefix-anchored patterns above (sk-, grn_, Bearer, JWT, ...) carry the
	// common carriers, and prompt-fencing is the backstop for the rest.
	regexp.MustCompile(`[A-Za-z0-9+/]{32,}={1,2}`),
	// Generic key=value assignments for secret-named keys (password, secret,
	// token, api_key, apikey, access_key) with a long-ish value.
	regexp.MustCompile(`(?i)(password|secret|token|api[_-]?key|access[_-]?key)\s*[:=]\s*['"]?[A-Za-z0-9_./+\-]{12,}['"]?`),
	// Credentials embedded in a connection-string URL userinfo
	// (scheme://user:pass@host or scheme://:pass@host) — DATABASE_URL,
	// REDIS_URL, mongodb+srv://, etc. The generic key=value pattern stops at the
	// "://", so the embedded password slips through without this. Redacts the
	// whole credentialed authority prefix up to and including the '@'.
	regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.\-]*://[^:@/\s]*:[^@/\s]{3,}@`),
}

// Scrubber redacts secret-shaped strings from untrusted text. The zero value is
// ready to use; NewScrubber exists for symmetry and future configurability.
type Scrubber struct{}

// NewScrubber returns a Scrubber.
func NewScrubber() *Scrubber { return &Scrubber{} }

// Scrub replaces every secret-shaped span in s with secretRedaction. It is
// idempotent and safe on already-clean text. For the key=value pattern the whole
// assignment is replaced so the secret value never survives.
func (s *Scrubber) Scrub(text string) string {
	out := text
	for _, p := range secretPatterns {
		out = p.ReplaceAllString(out, secretRedaction)
	}
	return out
}
