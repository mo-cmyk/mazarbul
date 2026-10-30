package chunk_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// TestEmbeddingsNeverEnterSync pins the structural guarantee that embeddings
// never cross the sync boundary. A sealed chunk serializes []kernel.Event;
// embeddings are a derived sqlite-vec projection keyed by entity UID —
// generated locally by the embedder, stored outside the event log, and never
// a field of an Event. So nothing the sealer can read carries a vector.
//
// If someone later adds an embedding-vector field to kernel.Event, the seal
// payload would silently start shipping float vectors to the remote (a
// privacy + chunk-size regression); this test fails first and flags it.
func TestEmbeddingsNeverEnterSync(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(kernel.Event{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if isFloatVector(f.Type) {
			t.Errorf("kernel.Event.%s is a float vector (%s): embeddings must never live on an Event — they would enter sealed chunks",
				f.Name, f.Type)
		}
		lower := strings.ToLower(f.Name)
		if strings.Contains(lower, "embed") || strings.Contains(lower, "vector") {
			t.Errorf("kernel.Event.%s looks like an embedding field; embeddings are a derived projection, never synced", f.Name)
		}
	}
}

func isFloatVector(t reflect.Type) bool {
	return t.Kind() == reflect.Slice &&
		(t.Elem().Kind() == reflect.Float32 || t.Elem().Kind() == reflect.Float64)
}
