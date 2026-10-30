package resolver

// predicates_lexicon_test.go pins the curated predicate lexicon consulted by
// autoRegisterPredicate before the LLM. In-package because lexiconLookup is
// deliberately unexported (an implementation detail of classification).

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
)

func TestLexiconLookup_HitsAndMisses(t *testing.T) {
	t.Parallel()
	kind, desc, ok := lexiconLookup("worked_at")
	if !ok {
		t.Fatal("worked_at must be in the lexicon")
	}
	if kind != NonFunctional {
		t.Errorf("worked_at kind = %q; past-tense predicates must be non-functional (auto-supersede would erase history)", kind)
	}
	if desc == "" {
		t.Error("lexicon entries must carry a description")
	}

	// Case-insensitive on the trimmed name.
	if _, _, ok := lexiconLookup("  Reports_To "); !ok {
		t.Error("lookup must be case-insensitive and trim whitespace")
	}

	if _, _, ok := lexiconLookup("invented_by_the_model"); ok {
		t.Error("unknown predicates must miss so the LLM path still classifies them")
	}
}

func TestLexicon_PastTenseNeverFunctional(t *testing.T) {
	t.Parallel()
	// Past-tense facts accumulate; a functional classification would make a
	// second past employer auto-supersede (close) the first.
	past := []string{"worked_at", "lived_in", "studied_at", "attended", "mentored", "visited", "traveled_to", "graduated_from"}
	for _, p := range past {
		kind, _, ok := lexiconLookup(p)
		if !ok {
			t.Errorf("%s missing from lexicon", p)
			continue
		}
		if kind != NonFunctional {
			t.Errorf("%s = %q, want non-functional", p, kind)
		}
	}
}

func TestLexicon_NeverShadowsMigrationSeeds(t *testing.T) {
	t.Parallel()
	// The 0006/0013 migrations own these; a lexicon duplicate would be dead
	// weight at best and a silent kind conflict at worst.
	seeded := []string{
		"works_at", "lives_in", "born_in", "child_of", "parent_of",
		"married_to", "member_of", "located_in", "has_role", "owns", "studies_at",
	}
	for _, p := range seeded {
		if _, _, ok := lexiconLookup(p); ok {
			t.Errorf("%s is migration-seeded and must not appear in the lexicon", p)
		}
	}
}

// TestAutoRegisterPredicate_LexiconSkipsLLM drives a lexicon predicate
// through the REAL autoRegisterPredicate path against a live SQLite store:
// no LLM call, row persisted with added_by='seed', PredicateRegistered event
// carries AddedBy=seed, and the registry cache resolves the kind.
func TestAutoRegisterPredicate_LexiconSkipsLLM(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	registry, err := NewRegistry(ctx, store.Reader())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	log := &captureLog{}
	chat := &countingChat{}
	r := &Resolver{
		eventLog:   log,
		chat:       chat,
		writer:     store.Writer(),
		predicates: registry,
		logger:     zap.NewNop(),
	}

	if err := r.autoRegisterPredicate(ctx, "worked_at"); err != nil {
		t.Fatalf("autoRegisterPredicate: %v", err)
	}

	if got := chat.toolUses.Load() + chat.completes.Load(); got != 0 {
		t.Fatalf("lexicon predicate must register with ZERO LLM calls, got %d", got)
	}
	kind, ok := registry.Kind("worked_at")
	if !ok || kind != NonFunctional {
		t.Errorf("registry.Kind(worked_at) = (%q, %v), want (non-functional, true)", kind, ok)
	}

	var addedBy string
	if err := store.Reader().QueryRowContext(ctx,
		`SELECT added_by FROM predicates WHERE predicate = 'worked_at'`,
	).Scan(&addedBy); err != nil {
		t.Fatalf("query predicates row: %v", err)
	}
	if addedBy != "seed" {
		t.Errorf("added_by = %q, want seed", addedBy)
	}

	if len(log.events) != 1 {
		t.Fatalf("want 1 PredicateRegistered event, got %d", len(log.events))
	}
	var ev ontology.PredicateRegisteredEvent
	if err := json.Unmarshal(log.events[0].Payload, &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if ev.Predicate != "worked_at" || ev.AddedBy != ontology.PredicateAddedBySeed {
		t.Errorf("event = {Predicate:%q AddedBy:%q}, want {worked_at seed}", ev.Predicate, ev.AddedBy)
	}
	if ev.Kind != ontology.PredicateKindNonFunctional {
		t.Errorf("event kind = %q, want non-functional", ev.Kind)
	}
}
