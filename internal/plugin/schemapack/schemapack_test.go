package schemapack_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/plugin/schemapack"
)

// recordingLog is a minimal kernel.EventLog that records appended events and
// stubs the read-side methods. Sufficient for exercising Installer.Install.
type recordingLog struct {
	appended []kernel.Event
}

func (l *recordingLog) Append(_ context.Context, events []kernel.Event) ([]kernel.Receipt, error) {
	l.appended = append(l.appended, events...)
	return make([]kernel.Receipt, len(events)), nil
}

func (*recordingLog) Read(_ context.Context, _ kernel.ReadOpts) iter.Seq2[kernel.Event, error] { //nolint:gocritic // EventLog interface requires value-typed ReadOpts param
	return func(func(kernel.Event, error) bool) {}
}

func (*recordingLog) At(_ context.Context, _ string) (kernel.Event, error) {
	return kernel.Event{}, kernel.ErrNotFound
}

func (*recordingLog) Rebuild(_ context.Context, _ kernel.Storage, _ ...kernel.RebuildOption) ([]kernel.ApplyError, error) {
	return nil, nil
}

func (*recordingLog) Provenance(_ context.Context, _ string) ([]kernel.ProvenanceRecord, error) {
	return nil, nil
}

// validPackFS builds an in-memory schema pack rooted at "pack" with one kind
// (carrying a propertySchema) and one predicate.
func validPackFS() fstest.MapFS {
	return fstest.MapFS{
		"pack/manifest.toml": &fstest.MapFile{Data: []byte(`
name = "demo"
version = "0.1.0"
description = "A demo pack."
category = "extension"
kind = "schema-pack"
`)},
		"pack/kinds/vehicle.yaml": &fstest.MapFile{Data: []byte(`
name: "extension:demo:vehicle"
displayName: "Vehicle"
description: "A motor vehicle."
extends: "thing"
sameAs:
  - "https://schema.org/Vehicle"
propertySchema:
  type: object
  properties:
    vin:
      type: string
`)},
		"pack/predicates.yaml": &fstest.MapFile{Data: []byte(`
predicates:
  - predicate: "manufactured_by"
    kind: "functional"
    description: "Subject vehicle is manufactured by the object organization."
    subjectKinds: ["extension:demo:vehicle"]
    objectKinds: ["organization"]
    sameAs: ["https://schema.org/manufacturer"]
`)},
	}
}

func TestParseFS_Valid(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.ParseFS(validPackFS(), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	if pack.Manifest.Name != "demo" || pack.Manifest.Version != "0.1.0" {
		t.Errorf("manifest = %+v, want name=demo version=0.1.0", pack.Manifest)
	}
	if len(pack.Kinds) != 1 || pack.Kinds[0].Name != "extension:demo:vehicle" {
		t.Fatalf("kinds = %+v, want 1 vehicle kind", pack.Kinds)
	}
	if pack.Kinds[0].Extends != "thing" || len(pack.Kinds[0].SameAs) != 1 {
		t.Errorf("kind extends/sameAs not parsed: %+v", pack.Kinds[0])
	}
	if len(pack.Predicates) != 1 || pack.Predicates[0].Predicate != "manufactured_by" {
		t.Fatalf("predicates = %+v, want 1 manufactured_by", pack.Predicates)
	}
	if pack.Predicates[0].Kind != "functional" {
		t.Errorf("predicate kind = %q, want functional", pack.Predicates[0].Kind)
	}
}

// TestParseFS_APIVersionAndScopes verifies the optional apiVersion + scopes
// manifest fields round-trip from manifest.toml into PackManifest, and that
// NewExtension propagates them onto the public plugin.Manifest. The fields are
// not gate-validated at parse time (that is the install gate's job); ParseFS
// must accept them verbatim, including values the gate would later reject.
func TestParseFS_APIVersionAndScopes(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"pack/manifest.toml": &fstest.MapFile{Data: []byte(`
name = "scoped"
version = "0.1.0"
description = "A pack that declares an apiVersion and scopes."
category = "extension"
kind = "schema-pack"
api_version = "v1.0.0"
scopes = ["project", "personal"]
`)},
	}
	pack, err := schemapack.ParseFS(fsys, "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	if pack.Manifest.APIVersion != "v1.0.0" {
		t.Errorf("APIVersion = %q, want v1.0.0", pack.Manifest.APIVersion)
	}
	if len(pack.Manifest.Scopes) != 2 || pack.Manifest.Scopes[0] != "project" || pack.Manifest.Scopes[1] != "personal" {
		t.Errorf("Scopes = %v, want [project personal]", pack.Manifest.Scopes)
	}

	ext := schemapack.NewExtension(pack)
	m := ext.Manifest()
	if m.APIVersion != "v1.0.0" {
		t.Errorf("plugin.Manifest.APIVersion = %q, want v1.0.0", m.APIVersion)
	}
	if len(m.Scopes) != 2 {
		t.Errorf("plugin.Manifest.Scopes = %v, want 2 scopes", m.Scopes)
	}
}

// TestParseFS_OmittedAPIVersionAndScopes verifies a manifest with neither new
// field parses to empty values (no error), so pre-existing packs stay valid.
func TestParseFS_OmittedAPIVersionAndScopes(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.ParseFS(validPackFS(), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	if pack.Manifest.APIVersion != "" {
		t.Errorf("APIVersion = %q, want empty", pack.Manifest.APIVersion)
	}
	if len(pack.Manifest.Scopes) != 0 {
		t.Errorf("Scopes = %v, want empty", pack.Manifest.Scopes)
	}
}

func TestParseFS_InvalidManifest(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing name":   "version=\"0.1.0\"\ncategory=\"extension\"\nkind=\"schema-pack\"\n",
		"wrong category": "name=\"x\"\nversion=\"0.1.0\"\ncategory=\"provider\"\nkind=\"schema-pack\"\n",
		"wrong kind":     "name=\"x\"\nversion=\"0.1.0\"\ncategory=\"extension\"\nkind=\"connector\"\n",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{"pack/manifest.toml": &fstest.MapFile{Data: []byte(manifest)}}
			if _, err := schemapack.ParseFS(fsys, "pack"); !errors.Is(err, schemapack.ErrInvalidManifest) {
				t.Errorf("err = %v, want ErrInvalidManifest", err)
			}
		})
	}
}

func TestParseFS_InvalidKind(t *testing.T) {
	t.Parallel()
	fsys := validPackFS()
	// Drop the required displayName.
	fsys["pack/kinds/vehicle.yaml"] = &fstest.MapFile{Data: []byte(
		"name: \"extension:demo:vehicle\"\ndescription: \"missing displayName\"\n")}
	if _, err := schemapack.ParseFS(fsys, "pack"); !errors.Is(err, schemapack.ErrInvalidKind) {
		t.Errorf("err = %v, want ErrInvalidKind", err)
	}
}

func TestParseFS_InvalidPredicate(t *testing.T) {
	t.Parallel()
	fsys := validPackFS()
	fsys["pack/predicates.yaml"] = &fstest.MapFile{Data: []byte(
		"predicates:\n  - predicate: \"x\"\n    kind: \"sometimes\"\n    description: \"bad kind\"\n")}
	if _, err := schemapack.ParseFS(fsys, "pack"); !errors.Is(err, schemapack.ErrInvalidPredicate) {
		t.Errorf("err = %v, want ErrInvalidPredicate", err)
	}
}

func TestParseFS_InvalidPropertySchema(t *testing.T) {
	t.Parallel()
	fsys := validPackFS()
	// {"type": 12} is valid YAML/JSON but not a compilable JSON Schema.
	fsys["pack/kinds/vehicle.yaml"] = &fstest.MapFile{Data: []byte(
		"name: \"extension:demo:vehicle\"\ndisplayName: \"V\"\ndescription: \"d\"\npropertySchema:\n  type: 12\n")}
	if _, err := schemapack.ParseFS(fsys, "pack"); err == nil {
		t.Fatal("expected error for uncompilable propertySchema, got nil")
	}
}

func TestParseFS_MissingManifest(t *testing.T) {
	t.Parallel()
	if _, err := schemapack.ParseFS(fstest.MapFS{}, "pack"); err == nil {
		t.Fatal("expected error for missing manifest, got nil")
	}
}

// TestLoadAllBuiltins is the golden test over the two shipped packs: it
// validates the embedded YAML data parses, every kind/predicate is well-formed,
// and the flagship kinds/predicates are present.
func TestLoadAllBuiltins(t *testing.T) {
	t.Parallel()
	packs, err := schemapack.LoadAllBuiltins()
	if err != nil {
		t.Fatalf("LoadAllBuiltins: %v", err)
	}
	if len(packs) != len(schemapack.BuiltinPackNames) {
		t.Fatalf("got %d packs, want %d", len(packs), len(schemapack.BuiltinPackNames))
	}
	byName := map[string]*schemapack.Pack{}
	for _, p := range packs {
		byName[p.Manifest.Name] = p
	}
	for _, want := range schemapack.BuiltinPackNames {
		p, ok := byName[want]
		if !ok {
			t.Fatalf("builtin pack %q missing from LoadAllBuiltins", want)
		}
		// agent-memory is an instructions-only steering preset and seeds no
		// kinds by design; every other builtin must declare at least one.
		if want == schemapack.DefaultSteeringPreset {
			continue
		}
		if len(p.Kinds) == 0 {
			t.Errorf("pack %q has no kinds", want)
		}
	}
	// business-de flagship must declare its German business kinds.
	bde := byName["business-de"]
	if bde == nil {
		t.Fatal("business-de pack missing")
	}
	hasCompany := false
	for _, k := range bde.Kinds {
		if k.Name == "" || k.DisplayName == "" || k.Description == "" {
			t.Errorf("business-de kind has empty required field: %+v", k)
		}
		if strings.Contains(k.Name, "company") {
			hasCompany = true
		}
	}
	if !hasCompany {
		t.Error("business-de pack does not declare a company kind")
	}
}

func TestInstall_DryRun(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.ParseFS(validPackFS(), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	log := &recordingLog{}
	inst := &schemapack.Installer{Log: log}
	res, err := inst.Install(context.Background(), pack, true)
	if err != nil {
		t.Fatalf("Install dry-run: %v", err)
	}
	if !res.DryRun {
		t.Error("result.DryRun = false, want true")
	}
	if len(res.KindsRegistered) != 1 || len(res.PredicatesRegistered) != 1 {
		t.Errorf("registered kinds=%v predicates=%v, want 1 each", res.KindsRegistered, res.PredicatesRegistered)
	}
	if len(log.appended) != 0 {
		t.Errorf("dry-run appended %d events, want 0", len(log.appended))
	}
}

func TestInstall_RealAppend(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.ParseFS(validPackFS(), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	log := &recordingLog{}
	inst := &schemapack.Installer{Log: log}
	res, err := inst.Install(context.Background(), pack, false)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.DryRun {
		t.Error("result.DryRun = true, want false")
	}
	// 1 kind + 1 predicate = 2 events.
	if len(log.appended) != 2 {
		t.Fatalf("appended %d events, want 2", len(log.appended))
	}
	var sawKind, sawPredicate bool
	for _, ev := range log.appended {
		switch string(ev.Kind) {
		case ontology.KindEntityKindRegistered:
			sawKind = true
			var k ontology.EntityKindRegisteredEvent
			if jerr := json.Unmarshal(ev.Payload, &k); jerr != nil {
				t.Fatalf("unmarshal kind payload: %v", jerr)
			}
			if k.Provenance != "extension:demo" {
				t.Errorf("kind provenance = %q, want extension:demo", k.Provenance)
			}
			if k.AddedBy != ontology.EntityKindAddedByExtension {
				t.Errorf("kind addedBy = %q, want extension", k.AddedBy)
			}
		case ontology.KindPredicateRegistered:
			sawPredicate = true
			var p ontology.PredicateRegisteredEvent
			if jerr := json.Unmarshal(ev.Payload, &p); jerr != nil {
				t.Fatalf("unmarshal predicate payload: %v", jerr)
			}
			if p.Kind != ontology.PredicateKindFunctional {
				t.Errorf("predicate kind = %q, want functional", p.Kind)
			}
			if len(p.SubjectKinds) != 1 || len(p.ObjectKinds) != 1 {
				t.Errorf("predicate domain/range not carried: %+v", p)
			}
		}
	}
	if !sawKind || !sawPredicate {
		t.Errorf("missing event kinds: sawKind=%v sawPredicate=%v", sawKind, sawPredicate)
	}
}

func TestLoadBuiltin_Unknown(t *testing.T) {
	t.Parallel()
	if _, err := schemapack.LoadBuiltin("nope"); !errors.Is(err, schemapack.ErrInvalidManifest) {
		t.Errorf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestExtension(t *testing.T) {
	t.Parallel()
	pack, err := schemapack.ParseFS(validPackFS(), "pack")
	if err != nil {
		t.Fatalf("ParseFS: %v", err)
	}
	ext := schemapack.NewExtension(pack)
	if ext.Kind() != schemapack.KindSchemaPack {
		t.Errorf("Kind() = %q, want %q", ext.Kind(), schemapack.KindSchemaPack)
	}
	if ext.Manifest().Name != "demo" {
		t.Errorf("Manifest().Name = %q, want demo", ext.Manifest().Name)
	}
	if err := ext.Init(context.Background(), nil); err != nil {
		t.Errorf("Init: %v", err)
	}
	if err := ext.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}
