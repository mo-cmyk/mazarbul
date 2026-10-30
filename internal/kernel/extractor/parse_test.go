package extractor

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseToolUseResponse_StringifiedMentions reproduces the llama3.2
// quirk where format:"json" mode returns nested arrays wrapped in a string
// literal. unwrapJSONField must unwrap the string once and decode the
// inner JSON, not bubble the unmarshal error up.
func TestParseToolUseResponse_StringifiedMentions(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": "[{\"kind\":\"person\",\"displayName\":\"Frodo\",\"extractionText\":\"Frodo\"}]",
	  "relations": []
	}`)
	mentions, candidates, err := ParseToolUseResponse(raw, "ep-1")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(mentions) != 1 {
		t.Fatalf("got %d mentions, want 1", len(mentions))
	}
	if mentions[0].DisplayName != "Frodo" {
		t.Errorf("displayName = %q, want Frodo", mentions[0].DisplayName)
	}
	if len(candidates) != 0 {
		t.Errorf("got %d candidates, want 0", len(candidates))
	}
}

func TestParseToolUseResponse_StringifiedRelations(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": [
	    {"kind":"person","displayName":"Merry","extractionText":"Merry"},
	    {"kind":"organization","displayName":"Mithril","extractionText":"Mithril"}
	  ],
	  "relations": "[{\"subjectMentionIdx\":0,\"predicate\":\"works_at\",\"objectMentionIdx\":1}]"
	}`)
	mentions, candidates, err := ParseToolUseResponse(raw, "ep-2")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(mentions) != 2 {
		t.Fatalf("got %d mentions, want 2", len(mentions))
	}
	if len(candidates) != 1 || candidates[0].Predicate != "works_at" {
		t.Fatalf("got %+v, want one works_at relation", candidates)
	}
}

func TestParseToolUseResponse_NullArrays(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"mentions": null, "relations": null}`)
	mentions, candidates, err := ParseToolUseResponse(raw, "ep-3")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(mentions) != 0 || len(candidates) != 0 {
		t.Errorf("expected empty slices on null arrays, got %d mentions / %d candidates", len(mentions), len(candidates))
	}
}

func TestParseToolUseResponse_BadInnerJSON(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"mentions": "not-json"}`)
	_, _, err := ParseToolUseResponse(raw, "ep-4")
	if err == nil {
		t.Fatal("expected error on malformed stringified mentions")
	}
	if !strings.Contains(err.Error(), "mentions") {
		t.Errorf("error should mention 'mentions': %v", err)
	}
}

// TestParseToolUseResponse_SingleQuotedInner covers the small-model quirk
// where the model emits Python-style single-quoted JSON inside the
// stringified wrapper. normaliseSingleQuotedJSON converts it on the second
// pass without losing single quotes inside double-quoted string literals.
func TestParseToolUseResponse_SingleQuotedInner(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"mentions": "[{'kind':'person','displayName':'Frodo','extractionText':'Frodo'}]","relations":[]}`)
	mentions, _, err := ParseToolUseResponse(raw, "ep-sq")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(mentions) != 1 || mentions[0].DisplayName != "Frodo" {
		t.Fatalf("got %+v, want one Frodo mention", mentions)
	}
}

// TestParseToolUseResponse_ParsesValidFromAndValidUntil covers the extract-v4
// addition: the model returns both validFrom and validUntil RFC 3339 strings
// and parse.go promotes them to *time.Time on the RelationCandidate so the
// resolver can write a bi-temporally bounded Relation.
func TestParseToolUseResponse_ParsesValidFromAndValidUntil(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": [
	    {"kind":"person","displayName":"Frodo","extractionText":"Frodo"},
	    {"kind":"organization","displayName":"Erebor","extractionText":"Erebor"}
	  ],
	  "relations": [
	    {
	      "subjectMentionIdx": 0,
	      "predicate": "works_at",
	      "objectMentionIdx": 1,
	      "validFrom": "2018-01-01T00:00:00Z",
	      "validUntil": "2024-12-31T23:59:59Z"
	    }
	  ]
	}`)
	_, candidates, err := ParseToolUseResponse(raw, "ep-vu")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].ValidFrom == nil || candidates[0].ValidFrom.Year() != 2018 {
		t.Errorf("ValidFrom = %v, want 2018-01-01", candidates[0].ValidFrom)
	}
	if candidates[0].ValidUntil == nil || candidates[0].ValidUntil.Year() != 2024 {
		t.Errorf("ValidUntil = %v, want 2024-12-31", candidates[0].ValidUntil)
	}
}

// TestParseToolUseResponse_TolerantOfBareYearValidUntil covers a known
// small-model failure mode: emitting just a year ("2020") in validUntil.
// parse.go must silently drop the bound rather than reject the relation; the
// resolver can still close it later via supersede-on-contradict.
func TestParseToolUseResponse_TolerantOfBareYearValidUntil(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": [
	    {"kind":"person","displayName":"Boromir","extractionText":"Boromir"},
	    {"kind":"place","displayName":"Berlin","extractionText":"Berlin"}
	  ],
	  "relations": [
	    {"subjectMentionIdx":0,"predicate":"lives_in","objectMentionIdx":1,"validUntil":"2020"}
	  ]
	}`)
	_, candidates, err := ParseToolUseResponse(raw, "ep-bad-vu")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].ValidUntil != nil {
		t.Errorf("ValidUntil should drop malformed bare year, got %v", candidates[0].ValidUntil)
	}
}

// TestParseToolUseResponse_ParsesRelationAttributes covers the v0.3.0
// schema addition: relations may carry an arbitrary string->string map
// of qualifying attributes (role, title, employment_type) extracted
// from the source text. parse.go must surface them on RelationCandidate
// so the resolver and graph lane can reason about role-qualified edges.
func TestParseToolUseResponse_ParsesRelationAttributes(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": [
	    {"kind":"person","displayName":"Rosie","extractionText":"Rosie"},
	    {"kind":"organization","displayName":"Erebor","extractionText":"Erebor"}
	  ],
	  "relations": [
	    {"subjectMentionIdx":0,"predicate":"works_at","objectMentionIdx":1,"attributes":{"role":"CEO","employment_type":"full-time"}},
	    {"subjectMentionIdx":0,"predicate":"has_role","objectMentionIdx":1,"attributes":{"role":"CEO"}}
	  ]
	}`)
	_, candidates, err := ParseToolUseResponse(raw, "ep-attrs")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(candidates))
	}
	if got := candidates[0].Attributes["role"]; got != "CEO" {
		t.Errorf("works_at attributes[role] = %q, want %q", got, "CEO")
	}
	if got := candidates[0].Attributes["employment_type"]; got != "full-time" {
		t.Errorf("works_at attributes[employment_type] = %q, want %q", got, "full-time")
	}
	if got := candidates[1].Attributes["role"]; got != "CEO" {
		t.Errorf("has_role attributes[role] = %q, want %q", got, "CEO")
	}
}

// TestParseToolUseResponse_StripsEmptyAttributeEntries pins the input-
// sanitisation contract: blank keys or blank values are dropped, and a
// fully-empty attributes object collapses to nil so callers don't have
// to special-case len(attrs)==0 vs attrs==nil.
func TestParseToolUseResponse_StripsEmptyAttributeEntries(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": [
	    {"kind":"person","displayName":"Rosie","extractionText":"Rosie"},
	    {"kind":"organization","displayName":"Erebor","extractionText":"Erebor"}
	  ],
	  "relations": [
	    {"subjectMentionIdx":0,"predicate":"works_at","objectMentionIdx":1,"attributes":{"":"oops","role":""}}
	  ]
	}`)
	_, candidates, err := ParseToolUseResponse(raw, "ep-empty-attrs")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].Attributes != nil {
		t.Errorf("Attributes should collapse to nil when every entry is empty, got %v", candidates[0].Attributes)
	}
}

// TestParseToolUseResponse_NoAttributesIsNotAnError pins the additive-
// schema contract: a relation without an attributes field still parses
// (v0.2.0 and earlier payloads keep working through M2+).
func TestParseToolUseResponse_NoAttributesIsNotAnError(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": [
	    {"kind":"person","displayName":"Frodo","extractionText":"Frodo"},
	    {"kind":"organization","displayName":"Erebor","extractionText":"Erebor"}
	  ],
	  "relations": [
	    {"subjectMentionIdx":0,"predicate":"works_at","objectMentionIdx":1}
	  ]
	}`)
	_, candidates, err := ParseToolUseResponse(raw, "ep-no-attrs")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	if candidates[0].Attributes != nil {
		t.Errorf("Attributes should be nil when absent from payload, got %v", candidates[0].Attributes)
	}
}

func TestNormaliseSingleQuotedJSON_PreservesSingleQuotesInsideDoubles(t *testing.T) {
	t.Parallel()
	in := `{"key":"O'Reilly"}`
	got := normaliseSingleQuotedJSON(in)
	if got != in {
		t.Errorf("got %q, want %q (apostrophes inside double-quoted strings must survive)", got, in)
	}
}

// TestParseEntityKind_OpenEnum covers the open-enum behaviour introduced in
// P2: the six core kinds fast-path to typed constants; other pattern-valid
// strings are accepted as custom kinds; pattern-invalid strings are rejected.
func TestParseEntityKind_OpenEnum(t *testing.T) {
	t.Parallel()

	type tc struct {
		input   string
		wantOK  bool
		wantOut string // only checked when wantOK==true
	}
	cases := []tc{
		// Six core kinds — fast-path, case-normalised.
		{"person", true, "person"},
		{"organization", true, "organization"},
		{"place", true, "place"},
		{"project", true, "project"},
		{"event", true, "event"},
		{"document", true, "document"},
		// Normalisation: uppercase, surrounding whitespace, interior spaces.
		{"Person", true, "person"},
		{" PLACE ", true, "place"},
		{"living space", true, "living_space"},
		// Custom / namespaced kinds.
		{"user:deal", true, "user:deal"},
		{"extension:schema_org:Person", true, "extension:schema_org:Person"},
		{"ns:sub_kind", true, "ns:sub_kind"},
		// Reject: empty, leading digit, leading colon, trailing colon.
		{"", false, ""},
		{"123abc", false, ""},
		{":deal", false, ""},
		{"user:", false, ""},
	}

	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			t.Parallel()
			got, err := parseEntityKind(c.input)
			if c.wantOK {
				if err != nil {
					t.Fatalf("parseEntityKind(%q) error = %v, want nil", c.input, err)
				}
				if string(got) != c.wantOut {
					t.Errorf("parseEntityKind(%q) = %q, want %q", c.input, got, c.wantOut)
				}
			} else {
				if err == nil {
					t.Errorf("parseEntityKind(%q) = %q, want error", c.input, got)
				}
			}
		})
	}
}

// TestParseToolUseResponse_CustomKindAccepted verifies that an unknown-but-
// pattern-valid kind flowing through ParseToolUseResponse is accepted
// (warn-accept policy) rather than silently dropping the mention.
func TestParseToolUseResponse_CustomKindAccepted(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{
	  "mentions": [{"kind":"user:deal","displayName":"Series A","extractionText":"Series A"}],
	  "relations": []
	}`)
	mentions, _, err := ParseToolUseResponse(raw, "ep-custom")
	if err != nil {
		t.Fatalf("ParseToolUseResponse: %v", err)
	}
	if len(mentions) != 1 {
		t.Fatalf("got %d mentions, want 1", len(mentions))
	}
	if string(mentions[0].Kind) != "user:deal" {
		t.Errorf("kind = %q, want user:deal", mentions[0].Kind)
	}
}
