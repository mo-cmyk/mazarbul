package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
)

// stubSelfBinder is a self->principal resolver whose result is fixed at
// construction: bound returns (principalUID, true); unbound returns ("", false).
type stubSelfBinder struct {
	principalUID string
	bound        bool
}

func (s stubSelfBinder) SelfPrincipal(_ context.Context) (string, bool) {
	return s.principalUID, s.bound
}

const testPrincipalUID = "01PRINCIPAL000000000000001"

// TestRelate_SelfRebindsToPrincipalWhenBound verifies that a relation whose
// subject or object is "self" persists the concrete principal UID once a binder
// reports the device bound — the team-sync contract (locked decision #3).
func TestRelate_SelfRebindsToPrincipalWhenBound(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})
	r.SetSelfBinder(stubSelfBinder{principalUID: testPrincipalUID, bound: true})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"mentions","object":"self"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	var rel struct {
		SubjectUID string `json:"subjectUid"`
		ObjectUID  string `json:"objectUid"`
	}
	if uerr := json.Unmarshal(app.events[0].Payload, &rel); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if rel.SubjectUID != testPrincipalUID {
		t.Errorf("subjectUid = %q, want rebound principal %q", rel.SubjectUID, testPrincipalUID)
	}
	if rel.ObjectUID != testPrincipalUID {
		t.Errorf("objectUid = %q, want rebound principal %q", rel.ObjectUID, testPrincipalUID)
	}
}

// TestRelate_SelfStaysLiteralWhenUnbound confirms the pre-identity behavior is
// preserved: an unbound device leaves "self" verbatim so existing single-user
// data and tests keep working.
func TestRelate_SelfStaysLiteralWhenUnbound(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	r := tools.NewRelate(app, relateEntityResolver{})
	r.SetSelfBinder(stubSelfBinder{bound: false})

	_, err := r.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","predicate":"lives_in","object":"01ARZ3NDEKTSV4RRFFQ69G5FB0"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var rel struct {
		SubjectUID string `json:"subjectUid"`
	}
	if uerr := json.Unmarshal(app.events[0].Payload, &rel); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if rel.SubjectUID != "self" {
		t.Errorf("subjectUid = %q, want literal self (unbound device)", rel.SubjectUID)
	}
}

// TestFact_SelfRebindsToPrincipalWhenBound mirrors the relate case for the fact
// tool's subject endpoint.
func TestFact_SelfRebindsToPrincipalWhenBound(t *testing.T) {
	t.Parallel()
	app := &captureRelateAppender{}
	f := tools.NewFact(app, relateEntityResolver{})
	f.SetSelfBinder(stubSelfBinder{principalUID: testPrincipalUID, bound: true})

	_, err := f.Invoke(context.Background(), json.RawMessage(
		`{"subject":"self","stateKey":"preference.coffee.milk","value":"oat"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(app.events) != 1 {
		t.Fatalf("appended %d events, want 1", len(app.events))
	}
	var fact struct {
		SubjectUID string `json:"subjectUid"`
	}
	if uerr := json.Unmarshal(app.events[0].Payload, &fact); uerr != nil {
		t.Fatalf("unmarshal payload: %v", uerr)
	}
	if fact.SubjectUID != testPrincipalUID {
		t.Errorf("subjectUid = %q, want rebound principal %q", fact.SubjectUID, testPrincipalUID)
	}
}
