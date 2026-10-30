package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel/navigate"
)

// shadowNavigator wraps fakeNavigator and implements the displayNameResolver
// interface (navigate.ResolveDisplayName) so the get tool's shadow-annotation
// path fires in unit tests without requiring a real SQLite database. It
// delegates to navigate.PartitionAndResolve, replicating the real Navigator's
// kind-partition logic so the shadow signal is produced correctly.
type shadowNavigator struct {
	*fakeNavigator
}

// ResolveDisplayName satisfies the displayNameResolver interface expected by
// resolveEntityByNameResult. It fetches exact-name candidates from the
// embedded fakeNavigator and delegates partition+tie-break to PartitionAndResolve,
// exercising the same logic the real *navigate.Navigator uses.
func (n *shadowNavigator) ResolveDisplayName(ctx context.Context, name string, _ navigate.ResolveMode, kindHint string) navigate.ResolveResult {
	const cap = 50
	ents, _ := n.ListEntities(ctx, navigate.EntityFilter{
		NameExact: name,
		Kind:      kindHint,
		Limit:     cap + 1,
	})
	overflow := len(ents) > cap
	if overflow {
		ents = ents[:cap]
	}
	return navigate.PartitionAndResolve(ents, "", overflow)
}

// TestGet_ShadowedDocument_Annotated is the happy-path shadow test: get("Mithrilmail")
// where a product wins over a same-named document must return the product AND
// populate shadowedDocuments with the document's resource name and uid.
func TestGet_ShadowedDocument_Annotated(t *testing.T) {
	t.Parallel()
	fake := &fakeNavigator{entities: []navigate.Entity{
		{UID: "product-uid", Kind: "product", DisplayName: "Mithrilmail"},
		{UID: "doc-uid", Kind: "document", DisplayName: "Mithrilmail"},
	}}
	g := NewGet(&shadowNavigator{fake})

	req, _ := json.Marshal(map[string]string{"name": "Mithrilmail"})
	body, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}

	t.Run("winner is the non-document entity", func(t *testing.T) {
		t.Parallel()
		if out.Type != "entity" || out.Entity == nil {
			t.Fatalf("expected type=entity with entity arm, got: %s", body)
		}
		if out.Entity.UID != "product-uid" {
			t.Fatalf("entity.uid = %q, want product-uid", out.Entity.UID)
		}
	})

	t.Run("shadowedDocuments carries the document identity", func(t *testing.T) {
		t.Parallel()
		if len(out.ShadowedDocuments) != 1 {
			t.Fatalf("shadowedDocuments len = %d, want 1; full response: %s", len(out.ShadowedDocuments), body)
		}
		got := out.ShadowedDocuments[0]
		if got.UID != "doc-uid" {
			t.Errorf("shadowedDocuments[0].uid = %q, want doc-uid", got.UID)
		}
		if got.Name != "entities/doc-uid" {
			t.Errorf("shadowedDocuments[0].name = %q, want entities/doc-uid", got.Name)
		}
	})
}

// TestGet_NoShadow_ShadowedDocumentsAbsent is the no-collision edge: a get
// that resolves to a lone non-document entity with no document namesake must
// NOT emit shadowedDocuments (not even an empty array — the field must be
// entirely absent from the JSON due to omitempty).
func TestGet_NoShadow_ShadowedDocumentsAbsent(t *testing.T) {
	t.Parallel()
	fake := &fakeNavigator{entities: []navigate.Entity{
		{UID: "org-uid", Kind: "organization", DisplayName: "Mithril"},
	}}
	g := NewGet(&shadowNavigator{fake})

	req, _ := json.Marshal(map[string]string{"name": "Mithril"})
	body, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var out getOutput
	if uerr := json.Unmarshal(body, &out); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if out.Type != "entity" || out.Entity == nil {
		t.Fatalf("expected entity arm, got: %s", body)
	}
	if len(out.ShadowedDocuments) != 0 {
		t.Fatalf("no name collision — shadowedDocuments must be empty, got: %v", out.ShadowedDocuments)
	}
	// Verify the key is absent from the JSON (omitempty contract).
	var raw map[string]json.RawMessage
	if uerr := json.Unmarshal(body, &raw); uerr != nil {
		t.Fatalf("re-decode: %v", uerr)
	}
	if _, present := raw["shadowedDocuments"]; present {
		t.Fatalf("shadowedDocuments key must be absent (omitempty) when empty, got %s", body)
	}
}

// TestGet_ResourceNameLookup_NoShadow ensures that a direct entities/{uid}
// resource-name lookup never emits shadowedDocuments even if a same-named
// document exists — the shadow signal is display-name-only.
func TestGet_ResourceNameLookup_NoShadow(t *testing.T) {
	t.Parallel()
	fake := &fakeNavigator{entities: []navigate.Entity{
		{UID: "product-uid", Kind: "product", DisplayName: "Mithrilmail"},
		{UID: "doc-uid", Kind: "document", DisplayName: "Mithrilmail"},
	}}
	g := NewGet(&shadowNavigator{fake})

	// Fetch by resource name: no display-name resolution, no shadow.
	req, _ := json.Marshal(map[string]string{"name": "entities/product-uid"})
	body, err := g.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var raw map[string]json.RawMessage
	if uerr := json.Unmarshal(body, &raw); uerr != nil {
		t.Fatalf("decode: %v", uerr)
	}
	if _, present := raw["shadowedDocuments"]; present {
		t.Fatalf("resource-name lookup must not emit shadowedDocuments, got %s", body)
	}
}
