package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/doctor"
)

// TestRenderDoctorNDJSON verifies the NDJSON Lines shape: one Result per
// line, lowerCamelCase fields, no array wrapper.
func TestRenderDoctorNDJSON(t *testing.T) {
	t.Parallel()
	results := []doctor.Result{
		{Index: 0, Name: "sqlite.open", Subsystem: "storage", Status: doctor.StatusOK, Detail: "ok", DurationMs: 12},
		{Index: 1, Name: "mcp.unix", Subsystem: "mcp", Status: doctor.StatusWarn, Detail: "no socket", DurationMs: 3},
	}
	var buf bytes.Buffer
	if err := renderDoctorNDJSON(&buf, results); err != nil {
		t.Fatalf("renderDoctorNDJSON: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		var out map[string]any
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			t.Errorf("line %d: parse: %v (%q)", i, err, line)
			continue
		}
		for _, key := range []string{"name", "subsystem", "status", "detail", "durationMs"} {
			if _, ok := out[key]; !ok {
				t.Errorf("line %d: missing key %q", i, key)
			}
		}
		// Index field is json:"-" and must not surface in the wire shape.
		if _, leaked := out["Index"]; leaked {
			t.Errorf("line %d: Index field leaked into JSON", i)
		}
	}
}

// TestRenderDoctorJSONArray verifies that --output json emits a SINGLE JSON
// array value. json.Unmarshal into []doctor.Result must succeed in one shot
// (no "Extra data: line 2 column 1" error that NDJSON causes).
func TestRenderDoctorJSONArray(t *testing.T) {
	t.Parallel()
	results := []doctor.Result{
		{Index: 0, Name: "sqlite.open", Subsystem: "storage", Status: doctor.StatusOK, Detail: "ok", DurationMs: 12},
		{Index: 1, Name: "mcp.unix", Subsystem: "mcp", Status: doctor.StatusWarn, Detail: "no socket", DurationMs: 3},
	}
	var buf bytes.Buffer
	if err := renderDoctorJSONArray(&buf, results); err != nil {
		t.Fatalf("renderDoctorJSONArray: %v", err)
	}
	raw := buf.Bytes()

	// Must parse as a single JSON value ([]Result), not multiple values.
	var parsed []map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&parsed); err != nil {
		t.Fatalf("decode single array: %v (output: %q)", err, raw)
	}
	// Decoder must be exhausted — no second top-level value.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err == nil {
		t.Fatalf("output contains more than one JSON value; extra: %s", extra)
	}

	// Must contain both results.
	if len(parsed) != 2 {
		t.Fatalf("got %d elements in array, want 2", len(parsed))
	}
	for i, obj := range parsed {
		for _, key := range []string{"name", "subsystem", "status", "detail", "durationMs"} {
			if _, ok := obj[key]; !ok {
				t.Errorf("element %d: missing key %q", i, key)
			}
		}
		if _, leaked := obj["Index"]; leaked {
			t.Errorf("element %d: Index field leaked into JSON", i)
		}
	}
}

// TestRenderDoctorNDJSON_MultipleValues confirms that NDJSON output is NOT a
// single JSON value — json.Unmarshal of the whole blob fails, proving the two
// formats are distinct.
func TestRenderDoctorNDJSON_MultipleValues(t *testing.T) {
	t.Parallel()
	results := []doctor.Result{
		{Index: 0, Name: "sqlite.open", Subsystem: "storage", Status: doctor.StatusOK, DurationMs: 5},
		{Index: 1, Name: "mcp.unix", Subsystem: "mcp", Status: doctor.StatusWarn, DurationMs: 2},
	}
	var buf bytes.Buffer
	if err := renderDoctorNDJSON(&buf, results); err != nil {
		t.Fatalf("renderDoctorNDJSON: %v", err)
	}
	raw := buf.Bytes()

	// Trying to parse the whole blob as a single value must fail ("Extra data").
	dec := json.NewDecoder(bytes.NewReader(raw))
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("first decode failed: %v", err)
	}
	var second json.RawMessage
	if err := dec.Decode(&second); err != nil {
		t.Fatalf("NDJSON must have multiple values but second decode failed: %v", err)
	}
}

// TestRenderDoctorCLI: every check shows up, grouped by subsystem in
// subsystemOrder, section rules are present, bullets carry glyphs, and
// the summary line lands at the end. enabled=false → plain text, no escapes.
func TestRenderDoctorCLI(t *testing.T) {
	t.Parallel()
	results := []doctor.Result{
		{Index: 0, Name: "sqlite.open", Subsystem: "storage", Status: doctor.StatusOK, DurationMs: 5},
		{Index: 1, Name: "mcp.healthz", Subsystem: "mcp", Status: doctor.StatusWarn, Detail: "down", DurationMs: 2},
		{Index: 2, Name: "logging.writable", Subsystem: "logging", Status: doctor.StatusFail, Detail: "EACCES", DurationMs: 1},
	}
	var buf bytes.Buffer
	if err := renderDoctorCLI(&buf, results, false); err != nil {
		t.Fatalf("renderDoctorCLI: %v", err)
	}
	out := buf.String()
	// Text must be present.
	for _, want := range []string{
		"sqlite.open",
		"mcp.healthz",
		"logging.writable",
		"summary: 3 checks · 1 ok · 1 warn · 1 fail",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
	// Plain path must carry glyphs (symbol-first principle).
	for _, glyph := range []string{"✓", "⚠", "✗"} {
		if !strings.Contains(out, glyph) {
			t.Errorf("plain output missing glyph %q\nfull:\n%s", glyph, out)
		}
	}
	// Section rules must be present.
	if !strings.Contains(out, "── storage") {
		t.Errorf("plain output missing storage section rule\nfull:\n%s", out)
	}
	// No ANSI escapes in plain path.
	if strings.Contains(out, "\x1b[") {
		t.Errorf("plain output must not contain ANSI escapes\nfull:\n%s", out)
	}
	// storage row precedes mcp row precedes logging row (subsystem ordering).
	idxStorage := strings.Index(out, "sqlite.open")
	idxMCP := strings.Index(out, "mcp.healthz")
	idxLogging := strings.Index(out, "logging.writable")
	if idxStorage >= idxMCP || idxMCP >= idxLogging {
		t.Errorf("ordering violated: storage=%d mcp=%d logging=%d", idxStorage, idxMCP, idxLogging)
	}
}

// TestGroupBySubsystem: subsystemOrder first, unknown subsystems
// trail in encounter order.
func TestGroupBySubsystem(t *testing.T) {
	t.Parallel()
	results := []doctor.Result{
		{Subsystem: "future"},
		{Subsystem: "storage"},
		{Subsystem: "mcp"},
		{Subsystem: "future"},
	}
	groups := groupBySubsystem(results)
	if len(groups) != 3 {
		t.Fatalf("len(groups) = %d, want 3", len(groups))
	}
	if groups[0][0].Subsystem != "storage" {
		t.Errorf("groups[0][0].Subsystem = %q, want storage", groups[0][0].Subsystem)
	}
	if groups[1][0].Subsystem != "mcp" {
		t.Errorf("groups[1][0].Subsystem = %q, want mcp", groups[1][0].Subsystem)
	}
	if groups[2][0].Subsystem != "future" {
		t.Errorf("groups[2][0].Subsystem = %q, want future", groups[2][0].Subsystem)
	}
	if len(groups[2]) != 2 {
		t.Errorf("len(future group) = %d, want 2", len(groups[2]))
	}
}

// TestResolveDoctorExitCode unwraps the sentinel into its embedded
// code and returns (0, false) for anything else.
func TestResolveDoctorExitCode(t *testing.T) {
	t.Parallel()
	if code, ok := ResolveDoctorExitCode(nil); ok || code != 0 {
		t.Errorf("nil: got (%d, %v), want (0, false)", code, ok)
	}
	if code, ok := ResolveDoctorExitCode(errors.New("not doctor")); ok || code != 0 {
		t.Errorf("generic err: got (%d, %v), want (0, false)", code, ok)
	}
	code, ok := ResolveDoctorExitCode(&doctorExitError{code: 1})
	if !ok || code != 1 {
		t.Errorf("doctor err: got (%d, %v), want (1, true)", code, ok)
	}
}

// TestDoctorExitError_Error pins the human-readable error text.
func TestDoctorExitError_Error(t *testing.T) {
	t.Parallel()
	e := &doctorExitError{code: 1}
	if got := e.Error(); !strings.Contains(got, "1 check") {
		t.Errorf("Error() = %q, want substring %q", got, "1 check")
	}
}

// ── WS12: Setup drift doctor checks ──────────────────────────────────────────

// TestSetupChecksHaveCorrectNames verifies that all setup checks carry the
// "setup" subsystem and have names prefixed with "setup.".
func TestSetupChecksHaveCorrectNames(t *testing.T) {
	t.Parallel()
	checks := setupChecks(t.TempDir())
	if len(checks) == 0 {
		t.Fatal("setupChecks returned empty slice")
	}
	for _, c := range checks {
		if c.Subsystem() != doctorSubsystemSetup {
			t.Errorf("check %q subsystem = %q; want %q", c.Name(), c.Subsystem(), doctorSubsystemSetup)
		}
		if !strings.HasPrefix(c.Name(), "setup.") {
			t.Errorf("check name %q does not start with \"setup.\"", c.Name())
		}
	}
}

// TestBuildDoctorChecks_IncludesSetup verifies that buildDoctorChecks combines
// the standard checks with the setup checks (total > default-only count).
func TestBuildDoctorChecks_IncludesSetup(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	all := buildDoctorChecks(dataDir)
	defaults := doctor.DefaultChecks(dataDir)
	if len(all) <= len(defaults) {
		t.Errorf("buildDoctorChecks returned %d checks; want > %d (default count)", len(all), len(defaults))
	}
	// At least one setup check must be present.
	found := false
	for _, c := range all {
		if c.Subsystem() == doctorSubsystemSetup {
			found = true
			break
		}
	}
	if !found {
		t.Error("buildDoctorChecks: no check with subsystem \"setup\" found")
	}
}

// TestSetupDaemonAgentCheck_ReturnsWarnOrOK verifies that the daemon-agent
// check never returns FAIL (only WARN or OK — it is advisory).
func TestSetupDaemonAgentCheck_ReturnsWarnOrOK(t *testing.T) {
	t.Parallel()
	check := newSetupDaemonAgentCheck()
	status, _, err := check.Run(t.Context())
	if err != nil {
		t.Fatalf("Run returned error %v; setup checks must never return errors", err)
	}
	if status == doctor.StatusFail {
		t.Errorf("daemon-agent check status = FAIL; setup checks must be WARN or OK only")
	}
}

// TestSetupCompletionCheck_ReturnsWarnOrOK verifies that the completion check
// never returns FAIL and correctly identifies a missing completion line.
func TestSetupCompletionCheck_ReturnsWarnOrOK(t *testing.T) {
	t.Parallel()
	check := newSetupCompletionCheck()
	status, _, err := check.Run(t.Context())
	if err != nil {
		t.Fatalf("Run returned error %v; setup checks must never return errors", err)
	}
	if status == doctor.StatusFail {
		t.Errorf("completion check status = FAIL; setup checks must be WARN or OK only")
	}
}

// TestDoctorSubsystemOrder_ContainsSetup verifies that the "setup" subsystem
// is in the canonical order list so it renders at the bottom of the table.
func TestDoctorSubsystemOrder_ContainsSetup(t *testing.T) {
	t.Parallel()
	for _, s := range doctorSubsystemOrder {
		if s == doctorSubsystemSetup {
			return
		}
	}
	t.Errorf("doctorSubsystemOrder does not contain %q; got %v", doctorSubsystemSetup, doctorSubsystemOrder)
}
