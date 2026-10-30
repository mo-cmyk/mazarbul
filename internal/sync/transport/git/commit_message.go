package git

// commit_message.go owns the structured commit message every sync push
// writes. The shape is the contract `maz blame`, `maz changelog`, and
// `maz sync log` parse — older parsers gate on the `Mazarbul-Schema`
// trailer so this file can evolve without breaking history surfaces.
//
// Two modes:
//
//   plaintext: subject carries semantic counts (`+3 episodes, +5
//     entities, …`) and the body enumerates every changed UID +
//     short payload preview. Mirrors `git log --stat`'s usefulness
//     for entity-level audit.
//
//   encrypted: subject reports only chunk counts and byte size; body
//     is empty save for `Chunks:` / `Device:` / `Mazarbul-Schema:`
//     trailers. The privacy contract: callers MUST pass Details=nil
//     for encrypted pushes; the renderer never invents details it
//     wasn't given, so a misconfigured caller cannot leak payloads.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mo-cmyk/mazarbul/internal/kernel"
	"github.com/mo-cmyk/mazarbul/internal/sync/chunk"
)

// commitSchemaTrailer is the version anchor parsers gate on. Bump the
// version when the body shape changes; existing parsers fall back to
// the legacy "(updated)" action when they see an unknown version.
const commitSchemaTrailer = "Mazarbul-Schema: sync-commit/v1"

// commitSectionMax caps each body section's row count. A `maz rebuild`
// can dump tens of thousands of entities into one push; without a cap
// the commit message would balloon into megabytes and bog down
// `git log`. 50 is the same threshold k9s uses for paginated tables.
const commitSectionMax = 50

// episodeSnippetMax is the per-episode preview length. Subject + 50
// episodes of 80 chars each = ~4 KiB, which is what we target for the
// upper bound on plaintext commits.
const episodeSnippetMax = 80

// actionModified is the neutral action label returned by actionForEntity
// when the entity was not enumerated in the structured body (renderer-only
// re-emit or pre-schema commit).
const actionModified = "modified"

// commitDetails is the plaintext-mode body payload. Empty in encrypted
// mode by construction: callers pass nil, the renderer skips body
// sections, and the privacy invariant is enforced by the absence of
// data, not by careful conditionals.
type commitDetails struct {
	Episodes  []episodeRef
	Entities  []entityChange
	Documents []documentChange
	Relations []relationChange
	Forgets   []forgetRef
}

type episodeRef struct {
	UID, Snippet string
}

type entityChange struct {
	UID, Kind, DisplayName string
	Action                 string // "+" added | "~" updated | "-" forgotten
}

type documentChange struct {
	UID, Path, Title string
	Action           string // "+" added | "~" revised | "-" deleted
}

type relationChange struct {
	SubjectUID, Predicate, ObjectUID string
	Confidence                       float64
	Action                           string // "+" | "~" | "-"
}

type forgetRef struct {
	Target string // entity UID or relation UID
}

// pushSummary bundles everything buildCommitMessage needs. Heads is the
// list of chunks pushed in this commit (already sorted by ULID by the
// caller, since the chunk store iterates in lex order). Details is
// nil-for-encrypted by the privacy contract.
type pushSummary struct {
	Scope     kernel.Scope
	DeviceID  string
	Tally     pushTally
	Heads     []chunk.Header
	SizeBytes int64
	Details   *commitDetails

	// Authorship trailers (Workstream A). PrincipalName/PrincipalEmail label the
	// committing user in the Principal: trailer (stable across the author-name
	// change). CommitMode records the resolved noreply|real|anonymous mode.
	// CoAuthors are the already-resolved "Name <email>" lines for distinct peer
	// principals in this (plaintext) push; Agents are the distinct event.Source
	// values. All are empty on encrypted pushes by the privacy contract — the
	// committing device still authors as its own principal, but peer/source
	// attribution requires decoding payloads, which encrypted mode forbids.
	PrincipalName  string
	PrincipalEmail string
	CommitMode     string
	CoAuthors      []string
	Agents         []string
	// peerPrincipalUIDs are the distinct non-self principal UIDs decoded from
	// this push's plaintext events, collected by buildPushSummary. Push resolves
	// them into the CoAuthors author lines (needs a ctx + resolver, which
	// buildPushSummary deliberately does not take).
	peerPrincipalUIDs []string
}

// buildCommitMessage renders the structured commit message. Subject is
// always present and ≤72 chars; body is present iff Details ≠ nil.
func buildCommitMessage(s pushSummary) string {
	var b strings.Builder
	b.WriteString(buildSubject(s))
	b.WriteString("\n\n")
	if s.Details != nil {
		writeDetailsSections(&b, s.Details)
	}
	writeTrailers(&b, s)
	return b.String()
}

// buildSubject returns the one-line summary. Plaintext pushes lead with
// semantic counts; encrypted pushes report only chunk count + bytes.
// Always stays within onelineMax (72) — if the count line would
// overflow, the ULID range gets dropped (it's also in the Chunks
// trailer) before falling back to a generic short form.
func buildSubject(s pushSummary) string {
	prefix := fmt.Sprintf("sync(%s): ", s.Scope)

	var counts string
	if s.Details != nil {
		counts = subjectCountsPlaintext(s.Details)
	} else {
		counts = subjectCountsSealed(s.Tally, s.SizeBytes)
	}
	rangeStr := ulidRangeShort(s.Heads)

	full := prefix + counts + " " + rangeStr
	if len(full) <= 72 {
		return full
	}
	withoutRange := prefix + counts
	if len(withoutRange) <= 72 {
		return withoutRange
	}
	// Wide batch (e.g. a `recreate`/`rebuild` that re-commits the whole
	// store at once): the full count line overflows the one-line budget.
	// Keep as many leading count fragments as fit before a "(N chunks)"
	// suffix so the subject still carries the headline figures; the body
	// enumerates the rest.
	return wideBatchSubject(prefix, s)
}

// wideBatchSubject builds the overflow subject: the leading "+N kind"
// fragments that fit before a " (N chunks)" suffix, falling back to a bare
// "(N chunks)" when not even one fragment fits or the push is encrypted
// (no decodable details).
func wideBatchSubject(prefix string, s pushSummary) string {
	suffix := fmt.Sprintf(" (%d chunks)", len(s.Heads))
	var parts []string
	if s.Details != nil {
		parts = subjectCountParts(s.Details)
	}
	kept := ""
	for _, p := range parts {
		candidate := p
		if kept != "" {
			candidate = kept + ", " + p
		}
		if len(prefix)+len(candidate)+len(suffix) > 72 {
			break
		}
		kept = candidate
	}
	if kept == "" {
		return fmt.Sprintf("%s(%d chunks)", prefix, len(s.Heads))
	}
	return prefix + kept + suffix
}

func subjectCountsPlaintext(d *commitDetails) string {
	parts := subjectCountParts(d)
	if len(parts) == 0 {
		return "rendered-only update"
	}
	return strings.Join(parts, ", ")
}

// subjectCountParts returns the per-kind "+N kind" fragments in subject
// order (episodes, entities, documents, relations, forgets), omitting any
// with a zero count.
func subjectCountParts(d *commitDetails) []string {
	parts := make([]string, 0, 5)
	if n := len(d.Episodes); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d %s", n, pluralize("episode", n)))
	}
	if n := len(d.Entities); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d %s", n, pluralize("entity", n)))
	}
	if n := len(d.Documents); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d %s", n, pluralize("document", n)))
	}
	if n := len(d.Relations); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d %s", n, pluralize("relation", n)))
	}
	if n := len(d.Forgets); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, pluralize("forget", n)))
	}
	return parts
}

func subjectCountsSealed(t pushTally, size int64) string {
	if t.encrypted == 0 {
		return fmt.Sprintf("+%d sealed chunks", t.encrypted)
	}
	return fmt.Sprintf("+%d sealed chunks (%s)", t.encrypted, humanBytes(size))
}

func writeDetailsSections(b *strings.Builder, d *commitDetails) {
	writeEpisodes(b, d.Episodes)
	writeEntities(b, d.Entities)
	writeDocuments(b, d.Documents)
	writeRelations(b, d.Relations)
	writeForgets(b, d.Forgets)
}

func writeEpisodes(b *strings.Builder, eps []episodeRef) {
	if len(eps) == 0 {
		return
	}
	b.WriteString("Episodes:\n")
	for i, e := range eps {
		if i >= commitSectionMax {
			fmt.Fprintf(b, "  … and %d more\n", len(eps)-commitSectionMax)
			break
		}
		fmt.Fprintf(b, "  + episodes/%s  %q\n", e.UID, truncate(e.Snippet, episodeSnippetMax))
	}
	b.WriteString("\n")
}

func writeEntities(b *strings.Builder, ents []entityChange) {
	if len(ents) == 0 {
		return
	}
	b.WriteString("Entities:\n")
	// Stable sort: action priority (forgotten > added > updated) then
	// kind then UID, so `git log -p` diffs read consistently.
	sort.SliceStable(ents, func(i, j int) bool {
		if ents[i].Action != ents[j].Action {
			return actionRank(ents[i].Action) < actionRank(ents[j].Action)
		}
		if ents[i].Kind != ents[j].Kind {
			return ents[i].Kind < ents[j].Kind
		}
		return ents[i].UID < ents[j].UID
	})
	for i, e := range ents {
		if i >= commitSectionMax {
			fmt.Fprintf(b, "  … and %d more\n", len(ents)-commitSectionMax)
			break
		}
		fmt.Fprintf(b, "  %s %s/%s  %s\n", e.Action, e.Kind, e.UID, e.DisplayName)
	}
	b.WriteString("\n")
}

func writeDocuments(b *strings.Builder, docs []documentChange) {
	if len(docs) == 0 {
		return
	}
	b.WriteString("Documents:\n")
	// Stable sort: action priority (deleted > added > revised) then UID, so
	// `git log -p` diffs read consistently across pushes.
	sort.SliceStable(docs, func(i, j int) bool {
		if docs[i].Action != docs[j].Action {
			return actionRank(docs[i].Action) < actionRank(docs[j].Action)
		}
		return docs[i].UID < docs[j].UID
	})
	for i, dc := range docs {
		if i >= commitSectionMax {
			fmt.Fprintf(b, "  … and %d more\n", len(docs)-commitSectionMax)
			break
		}
		label := dc.Path
		if label == "" {
			label = dc.Title
		}
		fmt.Fprintf(b, "  %s documents/%s  %q\n", dc.Action, dc.UID, label)
	}
	b.WriteString("\n")
}

func writeRelations(b *strings.Builder, rels []relationChange) {
	if len(rels) == 0 {
		return
	}
	b.WriteString("Relations:\n")
	for i, r := range rels {
		if i >= commitSectionMax {
			fmt.Fprintf(b, "  … and %d more\n", len(rels)-commitSectionMax)
			break
		}
		fmt.Fprintf(b, "  %s %s --[%s]--> %s  (conf %.2f)\n",
			r.Action, r.SubjectUID, r.Predicate, r.ObjectUID, r.Confidence)
	}
	b.WriteString("\n")
}

func writeForgets(b *strings.Builder, fs []forgetRef) {
	if len(fs) == 0 {
		return
	}
	b.WriteString("Forgets:\n")
	for i, f := range fs {
		if i >= commitSectionMax {
			fmt.Fprintf(b, "  … and %d more\n", len(fs)-commitSectionMax)
			break
		}
		fmt.Fprintf(b, "  - %s\n", f.Target)
	}
	b.WriteString("\n")
}

func writeTrailers(b *strings.Builder, s pushSummary) {
	chunkLine := fmt.Sprintf("Chunks: %d %s", len(s.Heads), chunkModeLabel(s.Tally, s.Heads))
	if s.SizeBytes > 0 {
		chunkLine += " (" + humanBytes(s.SizeBytes) + ")"
	}
	if r := ulidRangeFull(s.Heads); r != "" {
		chunkLine += " " + r
	}
	fmt.Fprintln(b, chunkLine)
	// Principal: the committing user, stable across an author-name change — the
	// identity surface Presence() keys on alongside Device:. Emitted whenever a
	// name is resolved (every mode but a totally headless sentinel push).
	if s.PrincipalName != "" {
		if s.PrincipalEmail != "" {
			fmt.Fprintf(b, "Principal: %s <%s>\n", s.PrincipalName, s.PrincipalEmail)
		} else {
			fmt.Fprintln(b, "Principal: "+s.PrincipalName)
		}
	}
	if s.DeviceID != "" {
		fmt.Fprintln(b, "Device: "+s.DeviceID)
	}
	// Co-authored-by: one per distinct PEER principal whose events rode along in
	// this plaintext push (already resolved + sorted by the caller). GitHub
	// renders these as additional contributors on the commit.
	for _, ca := range s.CoAuthors {
		fmt.Fprintln(b, "Co-authored-by: "+ca)
	}
	// Agent: the distinct event sources (mcp, connector:…) behind the push, so a
	// reader can tell agent-authored memory from a hand edit.
	if len(s.Agents) > 0 {
		fmt.Fprintln(b, "Agent: "+strings.Join(s.Agents, ", "))
	}
	if s.CommitMode != "" {
		fmt.Fprintln(b, "Mazarbul-Identity: "+s.CommitMode)
	}
	fmt.Fprintln(b, commitSchemaTrailer)
}

// chunkModeLabel says "plaintext" / "encrypted" / "mixed". Encrypted
// mode's label says "encrypted (no payload details exposed)" so anyone
// reading the commit knows why the body is sparse — that's the
// privacy explanation, not a missing-data warning.
func chunkModeLabel(t pushTally, heads []chunk.Header) string {
	plaintext, encrypted := 0, 0
	if len(heads) > 0 {
		for _, h := range heads {
			if h.Encrypted {
				encrypted++
			} else {
				plaintext++
			}
		}
	} else {
		plaintext, encrypted = t.plaintext, t.encrypted
	}
	switch {
	case encrypted > 0 && plaintext == 0:
		return "encrypted (no payload details exposed)"
	case plaintext > 0 && encrypted == 0:
		return "plaintext"
	default:
		return fmt.Sprintf("mixed (%d plaintext, %d encrypted)", plaintext, encrypted)
	}
}

func ulidRangeShort(heads []chunk.Header) string {
	if len(heads) == 0 {
		return ""
	}
	first := heads[0].FirstULID
	last := heads[len(heads)-1].LastULID
	if first == "" || last == "" {
		return ""
	}
	return fmt.Sprintf("[%s..%s]", first[:6], last[:6])
}

func ulidRangeFull(heads []chunk.Header) string {
	if len(heads) == 0 {
		return ""
	}
	first := heads[0].ChunkID
	last := heads[len(heads)-1].ChunkID
	if first == "" || last == "" {
		return ""
	}
	if first == last {
		return first
	}
	return first + ".." + last
}

func actionRank(a string) int {
	switch a {
	case "-":
		return 0
	case "+":
		return 1
	case "~":
		return 2
	default:
		return 3
	}
}

// actionForEntity walks a structured commit body and returns the
// blame-ready action string for the given entity UID. Used by `maz
// blame <entity>` to extract per-entity intent from the per-commit body.
//
// Returns "added" | "updated" | "forgotten" for the structured-body
// case and the neutral "modified" when the entity is not enumerated
// (renderer-only re-emits, or pre-schema commits).
func actionForEntity(commitBody, entityUID string) string {
	if entityUID == "" {
		return actionModified
	}
	// Each entity line has the shape:  `  + person/01KT8X0SERT5...  Frodo`
	// We accept any kind so the parser stays predicate-agnostic.
	for _, line := range strings.Split(commitBody, "\n") {
		if !strings.Contains(line, entityUID) {
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		if len(trimmed) < 2 {
			continue
		}
		switch trimmed[0] {
		case '+':
			return "added"
		case '~':
			return "updated"
		case '-':
			return "forgotten"
		}
	}
	return actionModified
}

func pluralize(noun string, n int) string {
	if n == 1 {
		return noun
	}
	switch noun {
	case "entity":
		return "entities"
	default:
		return noun + "s"
	}
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	default:
		return fmt.Sprintf("%.1f GiB", float64(n)/(k*k*k))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
