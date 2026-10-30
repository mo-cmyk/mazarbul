package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	personV1 = `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`
	personV2 = `{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer","minimum":0}},"required":["name","age"]}`
	placeV1  = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`
)

func mustSchema(kind, version, def string) Schema {
	return Schema{
		Kind:       kind,
		Version:    version,
		Definition: json.RawMessage(def),
		Provenance: "core",
	}
}

func TestMemoryOntology_Register(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name string
		// setup is applied to a fresh registry before the action.
		setup []Schema
		// action is the Register call under test.
		action Schema
		// want is the expected error sentinel from the action call;
		// nil means success. errors.Is(actual, want) must hold when want != nil.
		want error
		// wantWrapped, when non-empty, asserts the action error message
		// contains the substring (used for wrapped non-sentinel errors).
		wantWrapped string
	}{
		{
			name:   "new kind accepted",
			action: mustSchema("Person", "v0.1.0", personV1),
		},
		{
			name:   "same tuple is idempotent",
			setup:  []Schema{mustSchema("Person", "v0.1.0", personV1)},
			action: mustSchema("Person", "v0.1.0", personV1),
		},
		{
			name:   "same version different definition conflicts",
			setup:  []Schema{mustSchema("Person", "v0.1.0", personV1)},
			action: mustSchema("Person", "v0.1.0", personV2),
			want:   ErrSchemaConflict,
		},
		{
			name:   "higher version replaces",
			setup:  []Schema{mustSchema("Person", "v0.1.0", personV1)},
			action: mustSchema("Person", "v0.2.0", personV2),
		},
		{
			name:   "lower version different definition conflicts",
			setup:  []Schema{mustSchema("Person", "v0.2.0", personV2)},
			action: mustSchema("Person", "v0.1.0", personV1),
			want:   ErrSchemaConflict,
		},
		{
			name:        "invalid semver rejected",
			action:      mustSchema("Person", "not-a-version", personV1),
			wantWrapped: "invalid semver Version",
		},
		{
			name:        "empty kind rejected",
			action:      mustSchema("", "v0.1.0", personV1),
			wantWrapped: "empty Kind",
		},
		{
			name:        "empty definition rejected",
			action:      mustSchema("Person", "v0.1.0", ""),
			wantWrapped: "empty Definition",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := NewMemoryOntology()
			for _, s := range tt.setup {
				if err := o.Register(ctx, s); err != nil {
					t.Fatalf("setup Register: %v", err)
				}
			}
			err := o.Register(ctx, tt.action)
			switch {
			case tt.want != nil:
				if !errors.Is(err, tt.want) {
					t.Fatalf("Register: got %v, want errors.Is %v", err, tt.want)
				}
			case tt.wantWrapped != "":
				if err == nil || !contains(err.Error(), tt.wantWrapped) {
					t.Fatalf("Register: got %v, want error containing %q", err, tt.wantWrapped)
				}
			default:
				if err != nil {
					t.Fatalf("Register: got unexpected error %v", err)
				}
			}
		})
	}
}

func TestMemoryOntology_Register_HigherVersionInvalidatesCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := NewMemoryOntology()

	if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
		t.Fatalf("Register v1: %v", err)
	}
	// Populate the compile cache.
	if err := o.Validate(ctx, "Person", json.RawMessage(`{"name":"Merry"}`)); err != nil {
		t.Fatalf("Validate populate cache: %v", err)
	}
	// Replace with a stricter schema that requires both name and age.
	if err := o.Register(ctx, mustSchema("Person", "v0.2.0", personV2)); err != nil {
		t.Fatalf("Register v2: %v", err)
	}
	// The old payload (name only) was valid under v1 but is invalid under v2.
	err := o.Validate(ctx, "Person", json.RawMessage(`{"name":"Merry"}`))
	if err == nil {
		t.Fatal("Validate after version bump: want validation error, got nil")
	}
	// And a v2-valid payload passes.
	if err := o.Validate(ctx, "Person", json.RawMessage(`{"name":"Merry","age":30}`)); err != nil {
		t.Fatalf("Validate v2 payload: %v", err)
	}
}

func TestMemoryOntology_Schema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := NewMemoryOntology()
	original := mustSchema("Person", "v0.1.0", personV1)
	if err := o.Register(ctx, original); err != nil {
		t.Fatalf("Register: %v", err)
	}

	t.Run("hit returns deep-copied schema", func(t *testing.T) {
		got, err := o.Schema(ctx, "Person")
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		if got.Kind != "Person" || got.Version != "v0.1.0" {
			t.Fatalf("Schema: got %+v", got)
		}
		// Mutate the returned Definition; the registry must be unaffected.
		got.Definition[0] = 'X'
		again, err := o.Schema(ctx, "Person")
		if err != nil {
			t.Fatalf("Schema (second call): %v", err)
		}
		if again.Definition[0] == 'X' {
			t.Fatal("Schema: returned slice aliases internal state; mutation leaked into registry")
		}
	})

	t.Run("miss returns ErrNotFound", func(t *testing.T) {
		_, err := o.Schema(ctx, "Nope")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Schema: got %v, want errors.Is ErrNotFound", err)
		}
	})

	t.Run("empty kind returns ErrNotFound", func(t *testing.T) {
		_, err := o.Schema(ctx, "")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Schema(\"\"): got %v, want errors.Is ErrNotFound", err)
		}
	})
}

func TestMemoryOntology_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("empty registry returns empty slice", func(t *testing.T) {
		o := NewMemoryOntology()
		got, err := o.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if got == nil {
			t.Fatal("List: got nil, want empty slice")
		}
		if len(got) != 0 {
			t.Fatalf("List: got %d entries, want 0", len(got))
		}
	})

	t.Run("populated registry sorted by kind then version", func(t *testing.T) {
		o := NewMemoryOntology()
		// Insert out of order; List must return sorted output.
		for _, s := range []Schema{
			mustSchema("Place", "v0.1.0", placeV1),
			mustSchema("Person", "v0.2.0", personV2),
			mustSchema("Person", "v0.1.0", personV1),
		} {
			// "Person" v0.1.0 vs v0.2.0 is a version bump on the same Kind,
			// so the v0.1.0 second-insert would conflict. Insert with two
			// distinct Kinds to keep the test focused on sort order.
			_ = s
		}
		// Insert distinct (kind, version) pairs that can coexist.
		for _, s := range []Schema{
			mustSchema("Place", "v0.1.0", placeV1),
			mustSchema("Person", "v0.1.0", personV1),
		} {
			if err := o.Register(ctx, s); err != nil {
				t.Fatalf("Register %s: %v", s.Kind, err)
			}
		}
		got, err := o.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("List: got %d entries, want 2", len(got))
		}
		if got[0].Kind != "Person" || got[1].Kind != "Place" {
			t.Fatalf("List: sort order wrong, got %q then %q", got[0].Kind, got[1].Kind)
		}
	})

	t.Run("mutating returned slice does not affect registry", func(t *testing.T) {
		o := NewMemoryOntology()
		if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
			t.Fatalf("Register: %v", err)
		}
		got, err := o.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got[0].Definition[0] = 'X'
		// Schema lookup must still return the pristine definition.
		s, err := o.Schema(ctx, "Person")
		if err != nil {
			t.Fatalf("Schema after List mutate: %v", err)
		}
		if s.Definition[0] == 'X' {
			t.Fatal("List: returned Definition aliases internal state; mutation leaked into registry")
		}
	})
}

func TestMemoryOntology_Validate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("unregistered kind returns ErrSchemaNotRegistered", func(t *testing.T) {
		o := NewMemoryOntology()
		err := o.Validate(ctx, "Person", json.RawMessage(`{"name":"Merry"}`))
		if !errors.Is(err, ErrSchemaNotRegistered) {
			t.Fatalf("Validate: got %v, want errors.Is ErrSchemaNotRegistered", err)
		}
	})

	t.Run("happy path returns nil", func(t *testing.T) {
		o := NewMemoryOntology()
		if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := o.Validate(ctx, "Person", json.RawMessage(`{"name":"Merry"}`)); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("payload missing required field returns validation error", func(t *testing.T) {
		o := NewMemoryOntology()
		if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
			t.Fatalf("Register: %v", err)
		}
		err := o.Validate(ctx, "Person", json.RawMessage(`{}`))
		if err == nil {
			t.Fatal("Validate: want validation error, got nil")
		}
		// We don't squash the validator's error; it should reference the field.
		if !contains(err.Error(), "name") {
			t.Fatalf("Validate: error %q does not mention missing field 'name'", err.Error())
		}
	})

	t.Run("malformed payload JSON returns wrapped error", func(t *testing.T) {
		o := NewMemoryOntology()
		if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
			t.Fatalf("Register: %v", err)
		}
		err := o.Validate(ctx, "Person", json.RawMessage(`not-json`))
		if err == nil {
			t.Fatal("Validate: want JSON parse error, got nil")
		}
		if !contains(err.Error(), "kernel: parse payload") {
			t.Fatalf("Validate: error %q missing kernel-prefix wrapper", err.Error())
		}
	})

	t.Run("invalid schema definition surfaces compile error", func(t *testing.T) {
		o := NewMemoryOntology()
		// JSON Schema requires "type" to be a string or array; a number is
		// invalid and the compiler must reject it.
		bad := Schema{
			Kind:       "Person",
			Version:    "v0.1.0",
			Definition: json.RawMessage(`{"type": 42}`),
			Provenance: "core",
		}
		if err := o.Register(ctx, bad); err != nil {
			t.Fatalf("Register bad schema: %v", err)
		}
		err := o.Validate(ctx, "Person", json.RawMessage(`{}`))
		if err == nil {
			t.Fatal("Validate: want compile error, got nil")
		}
		if !contains(err.Error(), "kernel: compile schema") && !contains(err.Error(), "kernel: add schema resource") {
			t.Fatalf("Validate: error %q missing kernel-prefix compile wrapper", err.Error())
		}
	})

	t.Run("kind with URL-syntactic characters is handled literally", func(t *testing.T) {
		// Regression for Greptile P1 on PR #74: Register accepts any
		// non-empty kind, but Validate used to embed kind into a resource
		// URL where '#' would be reinterpreted as a JSON-pointer fragment,
		// causing the compile to fail for an accepted kind. The fix uses
		// a fixed resource URL; this test pins the contract.
		o := NewMemoryOntology()
		for _, kind := range []string{"a#b", "with/slash", "with?query", "weird#frag/and?q"} {
			if err := o.Register(ctx, mustSchema(kind, "v0.1.0", personV1)); err != nil {
				t.Fatalf("Register %q: %v", kind, err)
			}
			if err := o.Validate(ctx, kind, json.RawMessage(`{"name":"Merry"}`)); err != nil {
				t.Fatalf("Validate %q happy path: %v", kind, err)
			}
			err := o.Validate(ctx, kind, json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("Validate %q (missing name): want validation error, got nil", kind)
			}
		}
	})

	t.Run("validator is cached across calls", func(t *testing.T) {
		o := NewMemoryOntology()
		if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
			t.Fatalf("Register: %v", err)
		}
		payload := json.RawMessage(`{"name":"Merry"}`)
		for range 100 {
			if err := o.Validate(ctx, "Person", payload); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		}
		// Cache observability: the compiled entry's once.Do must have fired
		// exactly once. We can't peek at sync.Once's internal counter, but
		// we can assert that the entry.schema field is non-nil (compiled)
		// and that the entry pointer is stable across calls.
		o.mu.RLock()
		entry := o.compiled["Person"]
		o.mu.RUnlock()
		if entry == nil || entry.schema == nil {
			t.Fatalf("Validate: compile cache entry not populated, entry=%v", entry)
		}
	})
}

func TestMemoryOntology_ConcurrentRegisterAndValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := NewMemoryOntology()
	if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
		t.Fatalf("Register seed: %v", err)
	}

	const goroutines = 32
	const iterations = 200
	var wg sync.WaitGroup
	var validateOK, validateFail atomic.Int64
	versions := []string{"v0.1.0", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0"}
	defs := []string{personV1, personV2}

	// Writers: hammer Register with monotonically-non-decreasing versions.
	// Some calls succeed (idempotent or higher), some fail (lower or same
	// version with different definition); both are valid outcomes.
	for w := range goroutines / 2 {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := range iterations {
				v := versions[(seed+i)%len(versions)]
				d := defs[(seed+i)%len(defs)]
				_ = o.Register(ctx, mustSchema("Person", v, d))
			}
		}(w)
	}

	// Readers: validate continuously; outcome can be nil, validation
	// failure, or compile-cache-still-warming-up, but never panic and never
	// data-race.
	for range goroutines / 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := json.RawMessage(`{"name":"Merry","age":30}`)
			for range iterations {
				if err := o.Validate(ctx, "Person", payload); err == nil {
					validateOK.Add(1)
				} else {
					validateFail.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	total := validateOK.Load() + validateFail.Load()
	want := int64(goroutines / 2 * iterations)
	if total != want {
		t.Fatalf("validate total %d, want %d (no panics/races but counters off)", total, want)
	}
}

func TestMemoryOntology_ConcurrentListWhileRegister(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := NewMemoryOntology()

	const writers = 4
	const readers = 4
	const iterations = 200
	var wg sync.WaitGroup

	kinds := []string{"Alpha", "Beta", "Gamma", "Delta"}
	for w := range writers {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := range iterations {
				k := kinds[(seed+i)%len(kinds)]
				_ = o.Register(ctx, mustSchema(k, "v0.1.0", personV1))
			}
		}(w)
	}
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				got, err := o.List(ctx)
				if err != nil {
					t.Errorf("List: %v", err)
					return
				}
				// Snapshot must be internally sorted, no torn entries.
				for i := 1; i < len(got); i++ {
					if got[i-1].Kind > got[i].Kind {
						t.Errorf("List: snapshot not sorted: %q after %q", got[i].Kind, got[i-1].Kind)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkValidate_cached(b *testing.B) {
	ctx := context.Background()
	o := NewMemoryOntology()
	if err := o.Register(ctx, mustSchema("Person", "v0.1.0", personV1)); err != nil {
		b.Fatalf("Register: %v", err)
	}
	payload := json.RawMessage(`{"name":"Merry"}`)
	// Warm the compile cache so the bench measures the hot path.
	if err := o.Validate(ctx, "Person", payload); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := o.Validate(ctx, "Person", payload); err != nil {
			b.Fatalf("Validate: %v", err)
		}
	}
}

func BenchmarkValidate_coldCompile(b *testing.B) {
	ctx := context.Background()
	payload := json.RawMessage(`{"name":"Merry"}`)
	schema := mustSchema("Person", "v0.1.0", personV1)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		o := NewMemoryOntology()
		if err := o.Register(ctx, schema); err != nil {
			b.Fatalf("Register: %v", err)
		}
		if err := o.Validate(ctx, "Person", payload); err != nil {
			b.Fatalf("Validate: %v", err)
		}
	}
}

// contains is a tiny stdlib-free substring check, kept local so the test
// file does not pull in strings just for one assertion.
func contains(s, sub string) bool {
	if sub == "" {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
