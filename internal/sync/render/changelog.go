package render

import (
	"fmt"
	"path"
	"strings"
)

// changelogFragment holds per-push diff counts for the changelog.
type changelogFragment struct {
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
	HeadULID            string
	Device              string
}

// writeChangelogFragment renders the per-push CHANGELOG fragment as markdown.
// The diff between priorState and current state drives the bullet list.
func writeChangelogFragment(state *GraphState, priorState *GraphState, push changelogFragment) []byte {
	var buf strings.Builder

	fmt.Fprintf(&buf, "# Push %s\n", push.HeadULID)
	if push.Device != "" {
		fmt.Fprintf(&buf, "\nDevice: %s\n", push.Device)
	}
	buf.WriteByte('\n')

	if push.Conflicts > 0 {
		fmt.Fprintf(&buf, "- ⚠ %d unresolved supersede conflict(s)\n", push.Conflicts)
	}

	writeEntityDiffs(&buf, state, priorState)
	writeDocumentDiffs(&buf, state, priorState)
	writeRelationDiffs(&buf, state, priorState)
	writePredicateDiffs(&buf, state, priorState)

	fmt.Fprintf(&buf,
		"\n---\n%d ⚠ · %d + entities · %d ~ entities · %d ↺ orphaned · %d + relations · %d ⤳ superseded · %d ⊗ forgotten · %d + predicates · %d + documents · %d ~ documents · %d ⊗ documents\n",
		push.Conflicts,
		push.EntitiesAdded,
		push.EntitiesChanged,
		push.EntitiesOrphaned,
		push.RelationsAdded,
		push.RelationsSuperseded,
		push.RelationsForgotten,
		push.PredicatesAdded,
		push.DocumentsAdded,
		push.DocumentsChanged,
		push.DocumentsDeleted,
	)

	return []byte(buf.String())
}

func writeEntityDiffs(buf *strings.Builder, state *GraphState, prior *GraphState) {
	for _, uid := range sortedKeys(state.Entities) {
		row := state.Entities[uid]
		if prior == nil {
			// Use the redacted display name so raw URIs never reach the
			// plaintext CHANGELOG for external_ref entities (#207 render rule).
			fmt.Fprintf(buf, "- + entity %s/%s  %q\n", string(row.Kind), uid, redactedExternalRefDisplayName(row))
			continue
		}
		priorRow, existed := prior.Entities[uid]
		if !existed {
			fmt.Fprintf(buf, "- + entity %s/%s  %q\n", string(row.Kind), uid, redactedExternalRefDisplayName(row))
			continue
		}
		writeEntityDiffLine(buf, uid, row, priorRow)
	}
}

func writeEntityDiffLine(buf *strings.Builder, uid string, row, priorRow *EntityRow) {
	if row.Orphaned && !priorRow.Orphaned {
		fmt.Fprintf(buf, "- ↺ entity %s/%s  no current mentions\n", string(row.Kind), uid)
		return
	}
	if entityChanged(row, priorRow) {
		fmt.Fprintf(buf, "- ~ entity %s/%s  %q\n", string(row.Kind), uid, redactedExternalRefDisplayName(row))
	}
}

// entityChanged reports whether any tracked field differs between two rows.
func entityChanged(a, b *EntityRow) bool {
	if a.DisplayName != b.DisplayName {
		return true
	}
	if a.Confidence != b.Confidence {
		return true
	}
	if a.ResolutionStatus != b.ResolutionStatus {
		return true
	}
	if len(a.Aliases) != len(b.Aliases) {
		return true
	}
	if len(a.Identifiers) != len(b.Identifiers) {
		return true
	}
	return len(a.MentionUIDs) != len(b.MentionUIDs)
}

func writeRelationDiffs(buf *strings.Builder, state *GraphState, prior *GraphState) {
	for _, uid := range sortedKeys(state.Relations) {
		r := state.Relations[uid]
		if prior == nil {
			writeNewRelationLine(buf, r)
			continue
		}
		priorR, existed := prior.Relations[uid]
		if !existed {
			writeNewRelationLine(buf, r)
			continue
		}
		writeRelationDiffLine(buf, r, priorR)
	}
}

func writeRelationDiffLine(buf *strings.Builder, r, priorR *RelationRow) {
	if r.SupersededByUID != "" && priorR.SupersededByUID == "" {
		fmt.Fprintf(buf, "- ⤳ relation %s %s → %s (superseded by %s)\n",
			r.SubjectUID, r.Predicate, r.ObjectUID, r.SupersededByUID)
		return
	}
	if r.Forgotten && !priorR.Forgotten {
		reason := r.ForgottenReason
		if reason == "" {
			reason = "no reason given"
		}
		fmt.Fprintf(buf, "- ⊗ relation %s %s → %s (forgotten: %s)\n",
			r.SubjectUID, r.Predicate, r.ObjectUID, reason)
		return
	}
	if r.ConflictWith != "" && priorR.ConflictWith == "" {
		fmt.Fprintf(buf, "- ⚠ relation %s %s conflict with %s\n",
			r.SubjectUID, r.Predicate, r.ConflictWith)
	}
}

func writeNewRelationLine(buf *strings.Builder, r *RelationRow) {
	if r.SupersededByUID != "" || r.Forgotten {
		return
	}
	fmt.Fprintf(buf, "- + relation %s %s → %s\n",
		r.SubjectUID, r.Predicate, r.ObjectUID)
}

func writePredicateDiffs(buf *strings.Builder, state *GraphState, prior *GraphState) {
	for _, pred := range sortedKeys(state.Predicates) {
		if prior != nil {
			if _, existed := prior.Predicates[pred]; existed {
				continue
			}
		}
		p := state.Predicates[pred]
		fmt.Fprintf(buf, "- + predicate %s (%s)\n", pred, string(p.Kind))
	}
}

// changelogFragmentPath returns the POSIX path for a per-push changelog file.
// Format: CHANGELOG/<YYYY>/<MM>/<headULID>__<deviceSlug>.md.
func changelogFragmentPath(headULID, device string, year, month int) string {
	deviceSlug := slugBase(device)
	filename := headULID + "__" + deviceSlug + ".md"
	return path.Join("CHANGELOG",
		fmt.Sprintf("%04d", year),
		fmt.Sprintf("%02d", month),
		filename)
}

// computeStats computes Stats by diffing state against priorState.
func computeStats(state *GraphState, prior *GraphState) Stats {
	var s Stats
	computeEntityStats(&s, state, prior)
	computeRelationStats(&s, state, prior)
	computePredicateStats(&s, state, prior)
	computeConflictStats(&s, state, prior)
	computeDocumentStats(&s, state, prior)
	return s
}

func computeEntityStats(s *Stats, state *GraphState, prior *GraphState) {
	for _, uid := range sortedKeys(state.Entities) {
		row := state.Entities[uid]
		if prior == nil {
			s.EntitiesAdded++
			if row.Orphaned {
				// First-render orphan: the entity arrived AND ended
				// orphaned in the same pass (e.g., an episodeForgotten
				// landed in the same event stream that introduced the
				// entity). Count it under both signals — Added captures
				// the appearance, Orphaned captures the operationally
				// interesting fact for `maz diff`.
				s.EntitiesOrphaned++
			}
			continue
		}
		priorRow, existed := prior.Entities[uid]
		if !existed {
			s.EntitiesAdded++
			if row.Orphaned {
				s.EntitiesOrphaned++
			}
			continue
		}
		if row.Orphaned && !priorRow.Orphaned {
			s.EntitiesOrphaned++
		} else if entityChanged(row, priorRow) {
			s.EntitiesChanged++
		}
	}
}

func computeRelationStats(s *Stats, state *GraphState, prior *GraphState) {
	for _, uid := range sortedKeys(state.Relations) {
		r := state.Relations[uid]
		if prior == nil {
			countRelationStat(s, r, nil)
			continue
		}
		priorR, existed := prior.Relations[uid]
		if !existed {
			countRelationStat(s, r, nil)
			continue
		}
		countRelationStat(s, r, priorR)
	}
}

func computePredicateStats(s *Stats, state *GraphState, prior *GraphState) {
	for _, pred := range sortedKeys(state.Predicates) {
		if prior == nil {
			s.PredicatesAdded++
			continue
		}
		if _, existed := prior.Predicates[pred]; !existed {
			s.PredicatesAdded++
		}
	}
}

func computeConflictStats(s *Stats, state *GraphState, prior *GraphState) {
	conflicts := 0
	for _, uid := range sortedKeys(state.Relations) {
		r := state.Relations[uid]
		if r.ConflictWith == "" {
			continue
		}
		if prior == nil {
			conflicts++
			continue
		}
		if priorR, ok := prior.Relations[uid]; ok && priorR.ConflictWith != "" {
			continue
		}
		conflicts++
	}
	// Each conflict pair is recorded on both relation rows.
	s.Conflicts = conflicts / 2
}

func countRelationStat(s *Stats, r, prior *RelationRow) {
	if prior == nil {
		if r.SupersededByUID != "" {
			s.RelationsSuperseded++
		} else if r.Forgotten {
			s.RelationsForgotten++
		} else {
			s.RelationsAdded++
		}
		return
	}
	if r.SupersededByUID != "" && prior.SupersededByUID == "" {
		s.RelationsSuperseded++
	}
	if r.Forgotten && !prior.Forgotten {
		s.RelationsForgotten++
	}
}

// buildChangelogFragment builds a changelogFragment from Stats and push metadata.
func buildChangelogFragment(stats Stats, headULID, device string) changelogFragment {
	return changelogFragment{
		EntitiesAdded:       stats.EntitiesAdded,
		EntitiesChanged:     stats.EntitiesChanged,
		EntitiesOrphaned:    stats.EntitiesOrphaned,
		RelationsAdded:      stats.RelationsAdded,
		RelationsSuperseded: stats.RelationsSuperseded,
		RelationsForgotten:  stats.RelationsForgotten,
		Conflicts:           stats.Conflicts,
		PredicatesAdded:     stats.PredicatesAdded,
		DocumentsAdded:      stats.DocumentsAdded,
		DocumentsChanged:    stats.DocumentsChanged,
		DocumentsDeleted:    stats.DocumentsDeleted,
		HeadULID:            headULID,
		Device:              device,
	}
}
