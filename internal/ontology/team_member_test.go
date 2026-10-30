package ontology_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestTeamMemberGranted_RegisterAndRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterTeamMemberGranted(ctx, o); err != nil {
		t.Fatalf("RegisterTeamMemberGranted: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindTeamMemberGranted)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionTeamMemberGranted {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionTeamMemberGranted)
	}
	if got.Provenance != "core" {
		t.Errorf("schema.Provenance = %q, want core", got.Provenance)
	}

	original := ontology.TeamMemberGrantedEvent{
		GranteePrincipalUID:  "01GRANTEE0000000000000001",
		GranteeWrapPublicKey: "cHViLWtleQ==",
		WrappedDEK:           "d3JhcHBlZC1kZWs=",
		DEKEpoch:             2,
		GrantedAt:            time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ontology.TeamMemberGrantedEvent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != original {
		t.Errorf("round-trip drift: %+v vs %+v", back, original)
	}
}

func TestTeamMemberRevoked_RegisterAndRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := kernel.NewMemoryOntology()
	if err := ontology.RegisterTeamMemberRevoked(ctx, o); err != nil {
		t.Fatalf("RegisterTeamMemberRevoked: %v", err)
	}
	got, err := o.Schema(ctx, ontology.KindTeamMemberRevoked)
	if err != nil {
		t.Fatalf("Schema lookup: %v", err)
	}
	if got.Version != ontology.VersionTeamMemberRevoked {
		t.Errorf("schema.Version = %q, want %q", got.Version, ontology.VersionTeamMemberRevoked)
	}

	original := ontology.TeamMemberRevokedEvent{
		RevokedPrincipalUID: "01REVOKED00000000000000001",
		Reason:              "left the team",
		RevokedAt:           time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ontology.TeamMemberRevokedEvent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != original {
		t.Errorf("round-trip drift: %+v vs %+v", back, original)
	}
}
