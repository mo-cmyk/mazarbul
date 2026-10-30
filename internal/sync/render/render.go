package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"sort"
	"time"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
)

// Action describes what happened to a file between renders.
type Action int

const (
	// ActionCreate means the file did not exist before.
	ActionCreate Action = iota
	// ActionUpdate means the file existed and its content changed.
	ActionUpdate
	// ActionDelete means the file existed before but is no longer produced.
	ActionDelete
)

// File is one output artifact produced by the renderer.
type File struct {
	// Path is the POSIX path relative to the scope repo root.
	Path string
	// Content is the file bytes.
	Content []byte
	// Action describes whether this is a create, update, or delete.
	Action Action
}

// Stats counts the semantic changes in one render pass.
type Stats struct {
	EntitiesAdded       int
	EntitiesChanged     int
	EntitiesOrphaned    int
	RelationsAdded      int
	RelationsSuperseded int
	RelationsForgotten  int
	Conflicts           int
	PredicatesAdded     int
	DocumentsAdded      int
	DocumentsChanged    int
	DocumentsDeleted    int
}

// View is the output of one Render call.
type View struct {
	// Files is the complete set of files to write/delete, sorted by Path.
	Files []File
	// Cursor is the last consumed event ULID.
	Cursor string
	// Stats holds change counts for the changelog and diagnostics.
	Stats Stats
}

// Input is the input to Render.
type Input struct {
	// Scope is the kernel scope being rendered.
	Scope kernel.Scope
	// Events is the canonical event sequence, CreateTime asc, ULID tiebreak.
	Events iter.Seq[kernel.Event]
	// HeadULID identifies this push for changelog naming.
	HeadULID string
	// Device is the originating device slug for changelog provenance.
	Device string
	// Devices is the distinct set of devices that sealed the chunks behind this
	// render, surfaced in DASHBOARD.md's "last synced from N devices" banner. It
	// is an INJECTED, non-pure value (derived from chunk headers by the
	// transport) kept out of the pure render path so the determinism guarantee
	// holds: same Events + same Devices → byte-identical output.
	Devices []string
	// RenderedAt is the wall-clock-free timestamp stamped onto DASHBOARD.md. It
	// is INJECTED from the head ULID's embedded time (headULIDTime) rather than
	// read from a clock, preserving byte-identical renders across paired devices.
	RenderedAt time.Time
	// PriorState is the state from the previous render, enabling incremental
	// diffing. Nil means full render with no prior state.
	PriorState *GraphState
}

// Render is the sole exported entry point. It consumes the event sequence,
// builds GraphState, and assembles a View containing all output files.
// Same input always produces byte-identical output.
func Render(in Input) (View, error) {
	state := newGraphState()
	if in.PriorState != nil {
		state = cloneGraphState(in.PriorState)
	}

	for ev := range in.Events {
		if _, err := state.apply(ev); err != nil {
			return View{}, fmt.Errorf("render: apply event %s: %w", ev.UID, err)
		}
	}

	prior, err := buildPriorEmission(in.PriorState)
	if err != nil {
		return View{}, err
	}
	stats := computeStats(state, in.PriorState)

	files, err := assembleFiles(state, in, stats, prior)
	if err != nil {
		return View{}, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	return View{
		Files:  files,
		Cursor: state.Cursor,
		Stats:  stats,
	}, nil
}

// priorEmission maps path -> content for files the previous render produced.
type priorEmission map[string][]byte

// buildPriorEmission re-renders the prior state to get its file set so we can
// diff. Returns nil when priorState is nil.
func buildPriorEmission(priorState *GraphState) (priorEmission, error) {
	em := make(priorEmission)
	if priorState == nil {
		// An empty (non-nil) emission is diff-equivalent to "no prior": every
		// current file reads as a create.
		return em, nil
	}
	for _, uid := range sortedKeys(priorState.Entities) {
		p := entityFilePath(uid)
		em[p] = writeEntityStubFull(priorState, uid)
	}
	em[renderedPath("entities.jsonl")] = writeEntitiesJSONL(priorState)
	em[renderedPath("relations.jsonl")] = writeRelationsJSONL(priorState)
	em[renderedPath("superseded-relations.jsonl")] = writeSupersededRelationsJSONL(priorState)
	em[renderedPath("forgotten-relations.jsonl")] = writeForgottenRelationsJSONL(priorState)
	// facts.jsonl and source-refs.jsonl have stable paths but are emitted only
	// when non-empty (mirroring assembleFiles). They must use the SAME len>0
	// guard here: adding an empty (nil) entry would make diffFiles see a prior
	// path with no current counterpart and emit a spurious ActionDelete. Without
	// these, an unchanged file is absent from prior and re-emitted as ActionCreate
	// on every incremental sync — a redundant git write.
	if factContent := writeFactsJSONL(priorState); len(factContent) > 0 {
		em[renderedPath("facts.jsonl")] = factContent
	}
	if srContent := writeSourceRefsJSONL(priorState); len(srContent) > 0 {
		em[renderedPath("source-refs.jsonl")] = srContent
	}
	collectDocumentFiles(em, priorState)
	for _, kind := range allKinds() {
		em[kindIndexPath(kind)] = writeKindIndex(priorState, kind)
	}
	if err := buildPriorOntologyEmission(em, priorState); err != nil {
		return nil, err
	}
	return em, nil
}

// assembleFiles builds the complete slice of File entries for this render.
func assembleFiles(state *GraphState, in Input, stats Stats, prior priorEmission) ([]File, error) {
	current := make(map[string][]byte)

	collectEntityFiles(current, state)
	collectDocumentFiles(current, state)
	collectIndexFiles(current, state)
	if err := collectOntologyFiles(current, state); err != nil {
		return nil, err
	}

	factContent := writeFactsJSONL(state)
	if len(factContent) > 0 {
		current[renderedPath("facts.jsonl")] = factContent
	}

	srContent := writeSourceRefsJSONL(state)
	if len(srContent) > 0 {
		current[renderedPath("source-refs.jsonl")] = srContent
	}

	if in.HeadULID != "" {
		frag := buildChangelogFragment(stats, in.HeadULID, in.Device)
		fragContent := writeChangelogFragment(state, in.PriorState, frag)
		year, month := yearMonthFromULID(in.HeadULID)
		fragPath := changelogFragmentPath(in.HeadULID, in.Device, year, month)
		current[fragPath] = fragContent
	}

	// DASHBOARD.md is the at-a-glance landing view, regenerated wholesale every
	// push (like the changelog fragment, it is deliberately absent from
	// buildPriorEmission so it always re-emits rather than diff-suppressing on an
	// unchanged graph — its RenderedAt/Devices banner is push-specific).
	current[dashboardPath] = writeDashboard(state, in, stats)

	return diffFiles(current, prior), nil
}

// collectEntityFiles populates current with entity stubs and JSONL indexes.
func collectEntityFiles(current map[string][]byte, state *GraphState) {
	for _, uid := range sortedKeys(state.Entities) {
		p := entityFilePath(uid)
		current[p] = writeEntityStubFull(state, uid)
	}
	current[renderedPath("entities.jsonl")] = writeEntitiesJSONL(state)
	current[renderedPath("relations.jsonl")] = writeRelationsJSONL(state)
	current[renderedPath("superseded-relations.jsonl")] = writeSupersededRelationsJSONL(state)
	current[renderedPath("forgotten-relations.jsonl")] = writeForgottenRelationsJSONL(state)
}

// collectIndexFiles populates current with per-kind index files.
func collectIndexFiles(current map[string][]byte, state *GraphState) {
	for _, kind := range allKinds() {
		current[kindIndexPath(kind)] = writeKindIndex(state, kind)
	}
}

// diffFiles returns the File slice by comparing current to prior.
func diffFiles(current map[string][]byte, prior priorEmission) []File {
	files := make([]File, 0, len(current))
	for p, content := range current {
		action := ActionCreate
		if priorContent, existed := prior[p]; existed {
			if bytesEqual(priorContent, content) {
				continue
			}
			action = ActionUpdate
		}
		files = append(files, File{Path: p, Content: content, Action: action})
	}
	for p := range prior {
		if _, stillExists := current[p]; !stillExists {
			files = append(files, File{Path: p, Action: ActionDelete})
		}
	}
	return files
}

// factJSONLRow is the stable wire shape for one line in facts.jsonl. Encoded via
// json.Marshal (not fmt %q) so control characters in value/stateKey produce
// valid JSON \u00NN escapes rather than Go-only \a/\v/\xNN escapes.
type factJSONLRow struct {
	UID        string `json:"uid"`
	SubjectUID string `json:"subjectUid"`
	StateKey   string `json:"stateKey"`
	Value      string `json:"value"`
	CreateTime string `json:"createTime"`
}

// writeFactsJSONL renders per-scope facts.jsonl. Returns nil when empty.
func writeFactsJSONL(state *GraphState) []byte {
	if len(state.Facts) == 0 {
		return nil
	}
	sorted := make([]*FactRow, len(state.Facts))
	copy(sorted, state.Facts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UID < sorted[j].UID })

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, f := range sorted {
		encodeJSONL(enc, factJSONLRow{
			UID:        f.UID,
			SubjectUID: f.SubjectUID,
			StateKey:   f.StateKey,
			Value:      f.Value,
			CreateTime: f.CreateTime.UTC().Format(time.RFC3339Nano),
		})
	}
	return buf.Bytes()
}

// bytesEqual reports whether a and b have identical content.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// yearMonthFromULID extracts year and month from the 48-bit timestamp embedded
// in the first 10 Crockford Base32 characters of a ULID.
func yearMonthFromULID(uid string) (year int, month int) {
	if len(uid) < 10 {
		return 2000, 1
	}
	var ts uint64
	for _, c := range uid[:10] {
		ts <<= 5
		ts |= uint64(crockfordDecode(byte(c)))
	}
	sec := int64(ts / 1000)
	t := time.Unix(sec, 0).UTC()
	return t.Year(), int(t.Month())
}

// crockfordDecode maps a single Crockford Base32 character to its 5-bit value.
// The alphabet excludes I, L, O and U, so the letter runs are not contiguous:
// after H (17) it is J,K (18,19), then M,N (20,21) — skipping L — then P..T
// (22..26) and V..Z (27..31). Treating J..N as a five-letter run would shift
// every value from M onward by one and corrupt the decoded ULID timestamp.
func crockfordDecode(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'H':
		return c - 'A' + 10
	case c == 'J' || c == 'K':
		return c - 'J' + 18
	case c == 'M' || c == 'N':
		return c - 'M' + 20
	case c >= 'P' && c <= 'T':
		return c - 'P' + 22
	case c >= 'V' && c <= 'Z':
		return c - 'V' + 27
	default:
		return 0
	}
}
