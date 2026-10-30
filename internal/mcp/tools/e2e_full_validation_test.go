//go:build integration

package tools_test

// e2e_full_validation_test.go drives the FULL memory stack against real
// Ollama (qwen2.5:7b + qwen3-embedding:4b) and inspects every layer after
// each remember:
//
//   1. SQLite relational layer — counts entity, mention, relation rows
//   2. sqlite-vec — counts vectors in episodes_vec, mentions_vec, entities_vec
//   3. LadybugDB graph — observed via Components.Graph > 0 on recall hits
//   4. FTS5 — observed via Components.FTS > 0 on recall hits
//   5. HybridQuery recall — runs the 4-lane fuse end-to-end
//   6. Composer ask — verifies the LLM-composed answer mentions expected entities
//
// Edge cases covered: multilingual (DE+EN), pronouns, ambiguous names,
// temporal supersede, multi-entity, short-text skip.
//
// Run with:
//
//	OLLAMA_BENCH=1 go test -tags 'fts5 integration' \
//	    -run='^TestE2E_FullValidation$' -v -timeout=30m \
//	    ./internal/mcp/tools/

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/kernel/cache"
	"github.com/mo-cmyk/mazarbul/internal/kernel/composer"
	"github.com/mo-cmyk/mazarbul/internal/kernel/extractor"
	"github.com/mo-cmyk/mazarbul/internal/kernel/orchestrator"
	"github.com/mo-cmyk/mazarbul/internal/kernel/resolver"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/dispatcher"
	"github.com/mo-cmyk/mazarbul/internal/kernel/storage/sqlite"
	"github.com/mo-cmyk/mazarbul/internal/mcp/tools"
	"github.com/mo-cmyk/mazarbul/internal/ontology"
	"github.com/mo-cmyk/mazarbul/internal/prompts"
	"github.com/mo-cmyk/mazarbul/internal/provider/ollama"
)

type validationCase struct {
	name           string
	remembers      []string
	query          string
	expectInAnswer string
	expectInRecall string
	minEpisodes    int
	minEntities    int
	// expectGraphLane asserts that at least one recall hit has
	// Components.Graph > 0. The graph lane only fires on stores rich
	// enough that the 1-hop / 2-hop Cypher traversal returns neighbours
	// beyond the seed entities; cases 01-08 are intentionally too tiny
	// to exercise it. Case 09 seeds a connected sub-graph and is the
	// only case that flips this on.
	expectGraphLane bool

	// expectSupersede asserts that at least one recall hit has
	// Hit.Superseded=true AND relations.valid_until IS NOT NULL for at
	// least one row. Only case 04 sets this. The chain that has to
	// hold for the assertion to pass:
	// 1) resolver detects the functional-predicate clash and emits a
	//    RelationRetractedEvent.
	// 2) dispatcher.applyRelationRetracted commits both the SQLite
	//    UPDATE and the LadybugDB Cypher (which needs timestamp() cast
	//    on validUntil).
	// 3) HybridQuery.markSuperseded flips Hit.Superseded on the
	//    historical episode.
	// 4) composer renders the [SUPERSEDED] marker so the LLM can
	//    apply the CURRENCY rule.
	// Any break in this chain leaves valid_until NULL and the gate
	// fires.
	expectSupersede bool

	// timestamps optionally overrides the CreateTime on each remember.
	// When non-nil, len(timestamps) must equal len(remembers); the runner
	// bypasses the remember MCP tool and appends an Episode event directly
	// so the test owns tx-time. Used by case 10 to seed two episodes weeks
	// apart so `asOf` actually has something to filter.
	timestamps []time.Time

	// asOf is the transaction-time anchor passed to RecallRequest.AsOf.
	// When non-zero, the runner asserts that the asOf-cutoff filter
	// excludes any hit whose origin Episode.CreateTime > asOf.
	asOf time.Time

	// expectAbsentInRecall is a substring that must NOT appear in any
	// recall snippet when asOf is set. Used to pin that the post-cutoff
	// fact got filtered out.
	expectAbsentInRecall string
}

type validationReport struct {
	name           string
	episodes       int
	entities       int
	mentions       int
	relations      int
	episodesVec    int
	mentionsVec    int
	entitiesVec    int
	recallHits     int
	graphLaneFired bool
	ftsLaneFired   bool
	vecLaneFired   bool
	recallTopUID   string
	recallSnippet  string
	answerOK       bool
	answerExcerpt  string
	// supersededRelations counts relations.valid_until-IS-NOT-NULL
	// rows after the case's remembers settle. Non-zero on case 04
	// means the resolver correctly flagged the old works_at fact as
	// historical and the markSuperseded SELECT will flip
	// Hit.Superseded for the corresponding episode.
	supersededRelations int
	// hitSuperseded mirrors Hit.Superseded per recall hit so the
	// per-case log shows whether the composer's CURRENCY rule fired
	// on the right entry.
	hitSuperseded []bool
	errs          []string
}

func (r *validationReport) addErr(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// TestE2E_FullValidation drives the full stack across edge cases. Each
// subtest runs against a fresh fixture so layers start empty.
func TestE2E_FullValidation(t *testing.T) {
	requireOllamaT(t)
	cases := []validationCase{
		{
			name:           "01_simple_english_fact",
			remembers:      []string{"Frodo Baggins works at Erebor Inc. based in Berlin since 2020."},
			query:          "Where does Frodo work?",
			expectInAnswer: "Erebor",
			expectInRecall: "Frodo",
			minEpisodes:    1,
			minEntities:    2,
		},
		{
			name:           "02_multilingual_german_fact",
			remembers:      []string{"Arwen arbeitet seit 2022 bei Rivendell in Zürich."},
			query:          "Wer arbeitet bei Rivendell?",
			expectInAnswer: "Arwen",
			expectInRecall: "Arwen",
			minEpisodes:    1,
			minEntities:    2,
		},
		{
			name:           "03_cross_lingual_de_fact_en_query",
			remembers:      []string{"Boromir arbeitet bei Isengard in München."},
			query:          "Who works at Isengard?",
			expectInAnswer: "Boromir",
			expectInRecall: "Boromir",
			minEpisodes:    1,
			minEntities:    2,
		},
		{
			name: "04_temporal_supersede",
			remembers: []string{
				"Eowyn works at Erebor Inc. in Berlin.",
				"Eowyn left Erebor and joined Rivendell in 2025.",
			},
			query:           "Where does Eowyn work now?",
			expectInAnswer:  "Rivendell",
			expectInRecall:  "Eowyn",
			minEpisodes:     2,
			minEntities:     2,
			expectSupersede: true,
		},
		{
			name:           "05_pronoun_resolution",
			remembers:      []string{"Faramir founded Mirkwood Labs in Vienna. He raised a seed round in 2024."},
			query:          "Who founded Mirkwood Labs?",
			expectInAnswer: "Faramir",
			expectInRecall: "Faramir",
			minEpisodes:    1,
			minEntities:    2,
		},
		{
			name: "06_multi_entity_episode",
			remembers: []string{
				"At the offsite, Rosie Cotton from Erebor Inc. met Merry Brandybuck from Rivendell and Pearl Took from Mirkwood Labs in Lisbon.",
			},
			query:          "Who did Rosie meet at the offsite?",
			expectInAnswer: "Merry",
			expectInRecall: "Rosie",
			minEpisodes:    1,
			minEntities:    4,
		},
		{
			name:           "07_short_text_skipped",
			remembers:      []string{"OK fine."},
			query:          "what was said?",
			expectInAnswer: "",
			expectInRecall: "",
			minEpisodes:    1,
			minEntities:    0,
		},
		{
			name: "08_ambiguous_same_first_name",
			remembers: []string{
				"Frodo Baggins is a backend engineer at Erebor Inc.",
				"Frodo Gardner is a data scientist at Rivendell.",
			},
			query:          "Tell me about Frodo Baggins.",
			expectInAnswer: "Baggins",
			expectInRecall: "Baggins",
			minEpisodes:    2,
			minEntities:    3,
		},
		{
			// Case 09 seeds a connected sub-graph of six episodes so the
			// LadybugDB Cypher traversal in the graph lane has neighbours
			// to surface beyond the seed entity. The recall pipeline runs
			// the 1-hop traversal from each candidate seed; with Erebor
			// Inc. linked to Frodo, Arwen, Boromir, Elanor, Berlin and
			// Lisbon via works_at / located_in / met_at edges, at least
			// one hit must carry Components.Graph > 0. Cases 01-08 are
			// data-driven silent on the graph lane (tiny stores yield
			// sparse graphs); this is the only case that proves the lane
			// wires through end-to-end.
			name: "09_graph_traversal",
			remembers: []string{
				"Frodo Baggins works at Erebor Inc. based in Berlin.",
				"Arwen works at Erebor Inc. as a product designer.",
				"Boromir is the CTO of Erebor Inc. since 2023.",
				"Erebor Inc. has an office in Lisbon.",
				"Frodo met Arwen at the Erebor offsite in Berlin.",
				"Boromir co-founded Erebor Inc. with Elanor in 2018.",
			},
			query:           "Tell me about the people at Erebor.",
			expectInAnswer:  "Erebor",
			expectInRecall:  "Erebor",
			minEpisodes:     6,
			minEntities:     6,
			expectGraphLane: true,
		},
		{
			// Case 10 pins bi-temporal as-of semantics: two episodes weeks
			// apart, recall with AsOf set to a date BETWEEN them must
			// return only the earlier fact. The newer fact's snippet must
			// not appear in any returned hit. Bypasses the remember MCP
			// tool to gain CreateTime control on each Episode event.
			name: "10_temporal_as_of",
			remembers: []string{
				"Eowyn works at Erebor Inc. in Berlin.",
				"Eowyn left Erebor and joined Rivendell in 2026.",
			},
			timestamps: []time.Time{
				time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC),
				time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			},
			asOf:                 time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			query:                "Where does Eowyn work?",
			expectInAnswer:       "Erebor",
			expectInRecall:       "Eowyn",
			expectAbsentInRecall: "Rivendell",
			minEpisodes:          2,
			minEntities:          2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := runValidationCase(t, tc)
			logReport(t, report)
		})
	}
}

func runValidationCase(t *testing.T, tc validationCase) *validationReport {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rep := &validationReport{name: tc.name}

	fx, reader := newValidationFixture(ctx, t)

	if len(tc.timestamps) > 0 {
		if len(tc.timestamps) != len(tc.remembers) {
			rep.addErr("timestamps len=%d but remembers len=%d", len(tc.timestamps), len(tc.remembers))
			return rep
		}
		for i, text := range tc.remembers {
			payload, err := json.Marshal(map[string]any{
				"name":       fmt.Sprintf("episodes/01HE2E0000000000000000%04d", i),
				"uid":        fmt.Sprintf("01HE2E0000000000000000%04d", i),
				"text":       text,
				"source":     "e2e-validation",
				"confidence": 1.0,
				"createTime": tc.timestamps[i].UTC().Format(time.RFC3339Nano),
			})
			if err != nil {
				rep.addErr("marshal episode[%d]: %v", i, err)
				return rep
			}
			ev := kernel.Event{
				Kind:       kernel.EventKind("episode"),
				Source:     "e2e-validation",
				Confidence: 1.0,
				CreateTime: tc.timestamps[i].UTC(),
				Payload:    payload,
			}
			if _, err := fx.orch.Log.Append(ctx, []kernel.Event{ev}); err != nil {
				rep.addErr("Append[%d]: %v", i, err)
				return rep
			}
		}
	} else {
		rememberTool := tools.NewRemember(fx.orch.Log)
		for i, text := range tc.remembers {
			args, _ := json.Marshal(map[string]any{
				"text":       text,
				"source":     "e2e-validation",
				"confidence": 1.0,
			})
			if _, err := rememberTool.Invoke(ctx, args); err != nil {
				rep.addErr("remember[%d]: %v", i, err)
				return rep
			}
		}
	}
	if err := fx.orch.Log.WaitForBackgroundWork(ctx); err != nil {
		rep.addErr("WaitForBackgroundWork: %v", err)
	}

	rep.episodes = countRows(ctx, t, reader, "SELECT COUNT(*) FROM episodes")
	rep.entities = countRows(ctx, t, reader, "SELECT COUNT(*) FROM entities")
	rep.mentions = countRows(ctx, t, reader, "SELECT COUNT(*) FROM mentions")
	rep.relations = countRows(ctx, t, reader, "SELECT COUNT(*) FROM relations")
	// Probe for the supersede signal. markSuperseded uses the same
	// predicate; if this is 0 on case 04, the resolver did not
	// emit a valid_until on the old works_at relation and the
	// composer's CURRENCY rule has nothing to fire on. If this is
	// > 0 but the composer still picks the older fact, the issue
	// is on the prompt or model side, not the storage side.
	rep.supersededRelations = countRows(ctx, t, reader,
		"SELECT COUNT(*) FROM relations WHERE valid_until IS NOT NULL")
	// Debug-dump relations for cases that opted into temporal probing.
	// Tagged inside the case fixture so the dump cost is paid only
	// where it earns its keep (supersede triage).
	if tc.name == "04_temporal_supersede" {
		dumpRelationsForDebug(ctx, t, reader)
	}
	rep.episodesVec = countRowsAllowMissing(ctx, t, reader, "SELECT COUNT(*) FROM episodes_vec")
	rep.mentionsVec = countRowsAllowMissing(ctx, t, reader, "SELECT COUNT(*) FROM mentions_vec")
	rep.entitiesVec = countRowsAllowMissing(ctx, t, reader, "SELECT COUNT(*) FROM entities_vec")

	if rep.episodes < tc.minEpisodes {
		rep.addErr("episodes=%d < expected min %d", rep.episodes, tc.minEpisodes)
	}
	if rep.entities < tc.minEntities {
		rep.addErr("entities=%d < expected min %d (extractor may have under-extracted)",
			rep.entities, tc.minEntities)
	}

	// Recall + per-lane probes via Components.{Vector, FTS, Graph}.
	// Recall returns iter.Seq2[Hit, error]; collect into a slice so we
	// can both inspect lane firings and assert content on the top hit.
	recallHits := make([]kernel.Hit, 0, 10)
	var recallErr error
	recallReq := kernel.RecallRequest{Query: tc.query, Limit: 10}
	if !tc.asOf.IsZero() {
		asOf := tc.asOf
		recallReq.AsOf = &asOf
	}
	for hit, err := range fx.hybrid.Recall(ctx, recallReq) {
		if err != nil {
			recallErr = err
			break
		}
		recallHits = append(recallHits, hit)
	}
	if recallErr != nil {
		rep.addErr("hybrid.Recall: %v", recallErr)
	} else {
		rep.recallHits = len(recallHits)
		// A lane can't have fired if its underlying index is empty.
		// Components.Vector can pick up a non-zero post-RRF score from
		// cross-lane fusion side effects even when the vec lane returned
		// nothing (case 07 anomaly); guard each lane reading by the
		// row-count check on its index.
		vecIndexHasRows := rep.episodesVec > 0 || rep.mentionsVec > 0 || rep.entitiesVec > 0
		for _, h := range recallHits {
			if vecIndexHasRows && h.Components.Vector > 0 {
				rep.vecLaneFired = true
			}
			if h.Components.FTS > 0 {
				rep.ftsLaneFired = true
			}
			if h.Components.Graph > 0 {
				rep.graphLaneFired = true
			}
		}
		if len(recallHits) > 0 {
			rep.recallTopUID = recallHits[0].UID
			rep.recallSnippet = recallHits[0].Snippet
			rep.hitSuperseded = make([]bool, len(recallHits))
			for i, h := range recallHits {
				rep.hitSuperseded[i] = h.Superseded
			}
			if tc.expectInRecall != "" && !strings.Contains(strings.ToLower(rep.recallSnippet), strings.ToLower(tc.expectInRecall)) {
				rep.addErr("recall top snippet missing %q: %q", tc.expectInRecall, rep.recallSnippet)
			}
			if tc.expectAbsentInRecall != "" {
				needle := strings.ToLower(tc.expectAbsentInRecall)
				for _, h := range recallHits {
					if strings.Contains(strings.ToLower(h.Snippet), needle) {
						rep.addErr("asOf filter leaked: %q must be absent from recall but found in snippet %q",
							tc.expectAbsentInRecall, truncate(h.Snippet, 80))
						break
					}
				}
			}
		} else if tc.expectInRecall != "" {
			rep.addErr("recall returned zero hits for %q", tc.query)
		}
		// Lane-firing expectations. We only assert positive expectations
		// (the case explicitly opted in) so the assertion is a tight
		// regression alarm: if the graph lane goes dark on case 09, it
		// is a real wiring regression, not data noise.
		if tc.expectGraphLane && !rep.graphLaneFired {
			rep.addErr("graph lane did not fire; expected Components.Graph > 0 on at least one hit")
		}
		// Supersede expectation: at least one relation closed AND at
		// least one Hit flagged. These two checks together guard the
		// full chain (resolver -> dispatcher -> markSuperseded). A
		// pass means the composer's [SUPERSEDED] marker rendered;
		// only the LLM-side answer-substring check remains as a soft
		// signal beyond that.
		if tc.expectSupersede {
			if rep.supersededRelations == 0 {
				rep.addErr("expected at least one row with valid_until set; resolver/dispatcher supersede chain is broken")
			}
			anyHitSuperseded := false
			for _, b := range rep.hitSuperseded {
				if b {
					anyHitSuperseded = true
					break
				}
			}
			if !anyHitSuperseded {
				rep.addErr("expected at least one Hit.Superseded=true; HybridQuery.markSuperseded did not flip")
			}
		}
	}

	askTool := tools.NewAsk(fx.hybrid, fx.composer)
	askArgs, _ := json.Marshal(map[string]any{"question": tc.query})
	rawAsk, err := askTool.Invoke(ctx, askArgs)
	if err != nil {
		rep.addErr("ask: %v", err)
		return rep
	}
	var ans composer.Answer
	if err := json.Unmarshal(rawAsk, &ans); err != nil {
		rep.addErr("ask unmarshal: %v", err)
	}
	rep.answerExcerpt = truncate(ans.Answer, 160)
	if tc.expectInAnswer != "" {
		if strings.Contains(strings.ToLower(ans.Answer), strings.ToLower(tc.expectInAnswer)) {
			rep.answerOK = true
		} else {
			rep.addErr("answer missing %q", tc.expectInAnswer)
		}
	} else {
		rep.answerOK = true
	}
	return rep
}

func logReport(t *testing.T, r *validationReport) {
	t.Helper()
	t.Logf("=== %s ===", r.name)
	t.Logf("  SQLite:   episodes=%d  entities=%d  mentions=%d  relations=%d  superseded_relations=%d",
		r.episodes, r.entities, r.mentions, r.relations, r.supersededRelations)
	t.Logf("  super:    hit_superseded=%v", r.hitSuperseded)
	t.Logf("  vec:      episodes_vec=%d  mentions_vec=%d  entities_vec=%d",
		r.episodesVec, r.mentionsVec, r.entitiesVec)
	t.Logf("  lanes:    vec=%v  fts=%v  graph=%v  total_hits=%d",
		r.vecLaneFired, r.ftsLaneFired, r.graphLaneFired, r.recallHits)
	t.Logf("  recall:   top_uid=%q  snippet=%q",
		r.recallTopUID, truncate(r.recallSnippet, 80))
	t.Logf("  ask:      ok=%v  answer=%q", r.answerOK, r.answerExcerpt)
	for _, e := range r.errs {
		t.Errorf("  ERR: %s", e)
	}
}

func countRows(ctx context.Context, t *testing.T, reader *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := reader.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// dumpRelationsForDebug prints every relation in the table to the test
// log as (subject_uid, predicate, object_uid, valid_from, valid_until).
// Used by case 04 to verify which predicates the extractor produced
// and whether the resolver flagged any of them as historical. Calls
// t.Logf, never t.Errorf, so the dump is informational, never a gate.
func dumpRelationsForDebug(ctx context.Context, t *testing.T, reader *sql.DB) {
	t.Helper()
	rows, err := reader.QueryContext(ctx,
		`SELECT uid, subject_uid, predicate, object_uid,
		        COALESCE(valid_from, ''), COALESCE(valid_until, ''),
		        COALESCE(superseded_by_uid, '')
		   FROM relations
		   ORDER BY create_time`)
	if err != nil {
		t.Logf("  (dump relations: query failed: %v)", err)
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var u, s, p, o, vf, vu, sby string
		if err := rows.Scan(&u, &s, &p, &o, &vf, &vu, &sby); err != nil {
			t.Logf("  (dump relations: scan failed: %v)", err)
			return
		}
		t.Logf("  rel:      uid=%s  %s --%s--> %s  valid_from=%q valid_until=%q superseded_by=%q",
			u, s, p, o, vf, vu, sby)
	}
	if err := rows.Err(); err != nil {
		t.Logf("  (dump relations: iterate failed: %v)", err)
	}
}

func countRowsAllowMissing(ctx context.Context, t *testing.T, reader *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := reader.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Logf("  (table for %q unavailable: %v)", q, err)
		return -1
	}
	return n
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// requireOllamaT mirrors the *testing.B requireOllama gate for *testing.T.
// Inline because requireOllama uses *testing.B (b.Skip) and testing.TB
// has the right shape but the function-signature change would ripple
// through the bench file; cheaper to duplicate the 6 lines.
func requireOllamaT(t *testing.T) {
	t.Helper()
	if os.Getenv("OLLAMA_BENCH") != "1" {
		t.Skip("OLLAMA_BENCH=1 not set; skipping full-stack validation")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, ollamaBenchURL+"/api/tags", nil)
	if err != nil {
		t.Skipf("probe build: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("Ollama probe failed (%s): %v", ollamaBenchURL, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("Ollama probe returned %d", resp.StatusCode)
	}
}

// newValidationFixture mirrors newOllamaFixture but for *testing.T and
// also returns the SQLite reader so the per-case validation can run
// direct COUNT queries against every relational/vec table.
func newValidationFixture(ctx context.Context, t *testing.T) (*ollamaFixture, *sql.DB) {
	t.Helper()
	dataDir := t.TempDir()

	client, err := ollama.New(ctx, ollama.Options{
		BaseURL:    ollamaBenchURL,
		EmbedModel: ollamaBenchEmbedModel,
		ChatModel:  ollamaBenchChatModel,
	})
	if err != nil {
		t.Fatalf("ollama.New: %v", err)
	}
	// maxConcurrent 1 matches ExtractionWorkers=1 below: this test serialises
	// extraction, so the chat slot count tracks it.
	chat, err := ollama.NewChatProvider(client, 1)
	if err != nil {
		t.Fatalf("ollama.NewChatProvider: %v", err)
	}

	ont := kernel.NewMemoryOntology()
	// RegisterRelationRetracted and RegisterPredicateRegistered must be
	// in this list. The resolver emits RelationRetractedEvent through
	// EventLog.Append whenever a functional-predicate contradiction is
	// detected; without the schema registration the Append call returns
	// "kernel: schema not registered" and the closing UPDATE never runs.
	// Case 04 (Eowyn left Erebor and joined Rivendell) is the
	// regression that surfaces it: both works_at rows stay open with
	// valid_until IS NULL, the composer's CURRENCY rule sees no
	// [SUPERSEDED] marker, and answers depend on LLM date inference
	// rather than the supersede signal. Production wiring registers
	// these via wiring_memory.go; this fixture must mirror that set.
	for _, reg := range []func(context.Context, kernel.Ontology) error{
		ontology.RegisterEpisode, ontology.RegisterEntity, ontology.RegisterMention,
		ontology.RegisterRelation, ontology.RegisterRelationCandidatesProposed,
		ontology.RegisterRelationRetracted, ontology.RegisterPredicateRegistered,
	} {
		if err := reg(ctx, ont); err != nil {
			t.Fatalf("ontology register: %v", err)
		}
	}

	orch, err := orchestrator.New(ctx, orchestrator.Config{
		DataDir:  dataDir,
		Ontology: ont,
		Embedder: client,
		Logger:   zap.NewNop(),
		// ExtractionWorkers=1 serialises extraction so episode N
		// finishes resolving before episode N+1 starts. With > 1 the
		// extractor for ep2 ("Eowyn left Erebor and joined Rivendell")
		// can finish before ep1's resolver fires; the supersede check
		// for ep2's works_at(Eowyn, Rivendell) then sees zero open
		// works_at relations and never closes the still-pending
		// works_at(Eowyn, Erebor) from ep1. The race manifests as case
		// 04 returning the wrong employer because both works_at rows
		// stay open (relations.valid_until IS NULL on the historical
		// fact). One worker per case is fine for E2E validation: the
		// test fixture is small and the ordering bug is the
		// regression we are guarding against.
		ExtractionWorkers: 1,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	t.Cleanup(func() { _ = orch.Close() })

	store, err := sqlite.Open(ctx, filepath.Join(dataDir, "index.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	vec, err := sqlite.OpenVector(ctx, store)
	if err != nil {
		t.Fatalf("OpenVector: %v", err)
	}
	t.Cleanup(func() { _ = vec.Close() })
	fts, err := store.OpenFTS(ctx)
	if err != nil {
		t.Fatalf("OpenFTS: %v", err)
	}
	t.Cleanup(func() { _ = fts.Close() })

	caching := cache.New(client, store.Reader(), store.Writer(), nil)
	t.Cleanup(func() { _ = caching.Close() })

	// Pull the LadybugDB graph conn off the orchestrator's dispatcher
	// so HybridQuery's graph lane has the same neighbour-traversal
	// surface the daemon wires in production (see
	// internal/daemon/wiring.go buildHybridQuery). Without this the
	// graph lane silently no-ops and case 09's graph_traversal
	// assertion can never pass.
	disp, ok := orch.Storage.(*dispatcher.Dispatcher)
	if !ok {
		t.Fatalf("orchestrator storage is %T, want *dispatcher.Dispatcher", orch.Storage)
	}
	graph, err := disp.OpenGraph(ctx)
	if err != nil {
		t.Fatalf("dispatcher OpenGraph: %v", err)
	}
	t.Cleanup(func() { _ = graph.Close() })

	hybrid, err := kernel.NewHybridQuery(caching, vec, fts, store.Reader(), kernel.WithGraphConn(graph))
	if err != nil {
		t.Fatalf("NewHybridQuery: %v", err)
	}

	reg := prompts.New(nil)
	if err := extractor.RegisterExtractPrompt(ctx, reg); err != nil {
		t.Fatalf("register extract: %v", err)
	}
	if err := resolver.RegisterPrompts(ctx, reg); err != nil {
		t.Fatalf("register resolver: %v", err)
	}
	if err := composer.RegisterComposePrompt(ctx, reg); err != nil {
		t.Fatalf("register compose: %v", err)
	}

	predicates, err := resolver.NewRegistry(ctx, store.Writer())
	if err != nil {
		t.Fatalf("predicates: %v", err)
	}
	kindRegistry, err := resolver.NewKindRegistry(ctx, store.Writer())
	if err != nil {
		t.Fatalf("kind registry: %v", err)
	}

	ext := extractor.New(chat, reg, extractor.Config{
		Model:          ollamaBenchChatModel,
		RecentEpisodes: extractor.NewSQLiteRecentEpisodes(store.Writer()),
	})
	res, err := resolver.New(resolver.Config{Model: ollamaBenchChatModel}, resolver.Deps{
		EventLog:     orch.Log,
		Embedder:     caching,
		Chat:         chat,
		Vec:          vec,
		Reader:       store.Reader(),
		Writer:       store.Writer(),
		Prompts:      reg,
		Predicates:   predicates,
		KindRegistry: kindRegistry,
		Logger:       zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	orch.Log.SetExtractor(ext)
	orch.Log.SetResolver(res)

	comp, err := composer.New(composer.Config{Model: ollamaBenchChatModel}, chat, reg, zap.NewNop())
	if err != nil {
		t.Fatalf("composer.New: %v", err)
	}

	fx := &ollamaFixture{
		orch:     orch,
		hybrid:   hybrid,
		composer: comp,
		chat:     chat,
		embed:    caching,
	}
	return fx, store.Reader()
}
