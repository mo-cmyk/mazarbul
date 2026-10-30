package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

// A display name with an internal slash — common for product/standard names
// ("Ring Mk 4000 / RFC 1149") — must NOT be mistaken for an AIP resource name.
const slashName = "Ring Mk 4000 / RFC 1149 Integration"

func TestLooksLikeAIPResourceName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg  string
		want bool
	}{
		{"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV", true},
		{"relations/01ARZ3NDEKTSV4RRFFQ69G5FAV", true},
		{"consolidatedMemories/01ARZ3NDEKTSV4RRFFQ69G5FAV", true},
		{slashName, false},                    // slashed DISPLAY name, prefix not a collection
		{"HQ / home base", false},             // ditto
		{"Samwise", false},                    // bare name
		{"01ARZ3NDEKTSV4RRFFQ69G5FAV", false}, // bare uid, no slash
		{"foo/bar", false},                    // unknown collection → treat as name
		{"", false},
	}
	for _, c := range cases {
		if got := looksLikeAIPResourceName(c.arg); got != c.want {
			t.Errorf("looksLikeAIPResourceName(%q) = %v, want %v", c.arg, got, c.want)
		}
	}
}

// TestResolveEntityArg_SlashedDisplayName is the regression for the systemic
// bug: a slashed display name used to short-circuit into the resource-name
// parser and fail with "unknown collection". It must now resolve by name.
func TestResolveEntityArg_SlashedDisplayName(t *testing.T) {
	t.Parallel()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	nav := &fakeNavigator{entities: []navigate.Entity{{UID: uid, DisplayName: slashName}}}

	got, err := resolveEntityArg(context.Background(), nav, nil, slashName)
	if err != nil {
		t.Fatalf("resolveEntityArg(%q) errored: %v (slashed display name must resolve by name)", slashName, err)
	}
	if got != uid {
		t.Errorf("resolveEntityArg(%q) = %q, want %q", slashName, got, uid)
	}

	// Control: a genuine resource name still parses to its uid.
	got, err = resolveEntityArg(context.Background(), nav, nil, "entities/"+uid)
	if err != nil {
		t.Fatalf("resolveEntityArg(entities/%s) errored: %v", uid, err)
	}
	if got != uid {
		t.Errorf("resolveEntityArg(entities/%s) = %q, want %q", uid, got, uid)
	}
}

// TestRelate_SlashedObjectName is the end-to-end regression on the relate path
// the user hit: relate("…", works_on, "Ring Mk 4000 / RFC 1149 Integration") used
// to fail with E_VALIDATION "unknown collection"; it must now resolve the object
// by name and write the edge.
func TestRelate_SlashedObjectName(t *testing.T) {
	t.Parallel()
	const objUID = "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	app := &captureAppender{}
	// entity != nil makes the endpoint existence check pass for any uid; the
	// entities slice drives display-name resolution via ListEntities.
	nav := &fakeNavigator{
		entity:   &navigate.Entity{UID: "endpoint"},
		entities: []navigate.Entity{{UID: objUID, DisplayName: slashName}},
	}
	r := NewRelate(app, nav)
	r.SetNameNavigator(nav)

	body, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"entities/01ARZ3NDEKTSV4RRFFQ69G5FAV","predicate":"works_on","object":"`+slashName+`"}`))
	if err != nil {
		t.Fatalf("relate with slashed object name errored: %v", err)
	}
	var out struct{ Status string }
	if uerr := json.Unmarshal(body, &out); uerr != nil || out.Status != "asserted" {
		t.Fatalf("relate output = %s (err %v), want status=asserted", body, uerr)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	var rel ontology.Relation
	if uerr := json.Unmarshal(app.events[0].Payload, &rel); uerr != nil {
		t.Fatalf("unmarshal relation: %v", uerr)
	}
	if rel.ObjectUID != objUID {
		t.Errorf("relation ObjectUID = %q, want %q (resolved from slashed name)", rel.ObjectUID, objUID)
	}
}
