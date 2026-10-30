package cognitive

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Decay sweep defaults (issue #231).
const (
	// DemoteFloorDefault is the effective-confidence floor below which a hot
	// insight is demoted out of recall.
	DemoteFloorDefault = 0.1

	// DecayHalflifeDaysDefault is σ (days) for the Gaussian confidence decay,
	// matching the recall recency/strength halflife.
	DecayHalflifeDaysDefault = 30.0
)

// SweepOptions parameterizes the insight decay/demote sweep.
type SweepOptions struct {
	// DemoteFloor is the effective-confidence threshold; below it, a hot
	// insight is demoted. Defaults to DemoteFloorDefault when <= 0.
	DemoteFloor float64
	// HalflifeDays is σ for the Gaussian decay. Defaults to
	// DecayHalflifeDaysDefault when <= 0.
	HalflifeDays float64
	// Now is the decay anchor; defaults to the caller's clock when zero.
	Now time.Time
}

// insightDecayRow is one hot insight's decay inputs.
type insightDecayRow struct {
	uid           string
	synthesizedAt time.Time
	lastAccessed  time.Time
	baseline      float64
}

// SweepInsightDecay lowers each hot insight's current_confidence by a Gaussian
// decay over days since its last access (or synthesis, if never accessed) and
// demotes (state='demoted') any insight whose effective confidence falls below
// the floor. This is PROJECTION-side state — it never appends an event — so
// `maz rebuild` replays only the synthesis event, resets current_confidence to
// synthesized_confidence and state to 'hot', and RESTORES a demoted insight to
// hot recall (the sweep has not run yet post-rebuild). Returns the number of
// insights newly demoted by this pass.
//
// A reinforcing access advances memory_strength.last_accessed_at, which shrinks
// the decay on the next sweep — so a re-accessed insight naturally re-promotes
// (the applyMemoryAccessed arm also clears state back to 'hot' immediately).
func SweepInsightDecay(ctx context.Context, db *sql.DB, opts SweepOptions) (int, error) {
	floor := opts.DemoteFloor
	if floor <= 0 {
		floor = DemoteFloorDefault
	}
	halflife := opts.HalflifeDays
	if halflife <= 0 {
		halflife = DecayHalflifeDaysDefault
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	candidates, err := loadHotInsights(ctx, db)
	if err != nil {
		return 0, err
	}

	demoted := 0
	for _, row := range candidates {
		anchor := row.lastAccessed
		if anchor.IsZero() {
			anchor = row.synthesizedAt
		}
		effective := row.baseline * gaussianDecay(now.Sub(anchor), halflife)
		state := "hot"
		if effective < floor {
			state = "demoted"
			demoted++
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE insights SET current_confidence = ?, state = ? WHERE uid = ?`,
			effective, state, row.uid,
		); err != nil {
			return demoted, fmt.Errorf("cognitive: decay sweep update %q: %w", row.uid, err)
		}
	}
	return demoted, nil
}

// loadHotInsights reads every hot insight's decay inputs, draining the cursor
// before any UPDATE so the write loop does not contend with an open read.
func loadHotInsights(ctx context.Context, db *sql.DB) ([]insightDecayRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT i.uid, i.synthesized_confidence, i.synthesized_at, ms.last_accessed_at
		   FROM insights i
		   LEFT JOIN memory_strength ms ON ms.target_uid = i.uid
		  WHERE i.state = 'hot'`)
	if err != nil {
		return nil, fmt.Errorf("cognitive: decay sweep query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []insightDecayRow
	for rows.Next() {
		var (
			r          insightDecayRow
			synthRaw   string
			lastAccess sql.NullString
		)
		if err := rows.Scan(&r.uid, &r.baseline, &synthRaw, &lastAccess); err != nil {
			return nil, fmt.Errorf("cognitive: decay sweep scan: %w", err)
		}
		if t, perr := time.Parse(time.RFC3339Nano, synthRaw); perr == nil {
			r.synthesizedAt = t.UTC()
		}
		if lastAccess.Valid && lastAccess.String != "" {
			if t, perr := time.Parse(time.RFC3339Nano, lastAccess.String); perr == nil {
				r.lastAccessed = t.UTC()
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cognitive: decay sweep iterate: %w", err)
	}
	return out, nil
}

// gaussianDecay returns exp(-(days²)/(2σ²)) ∈ (0, 1], the same kernel the recall
// recency boost uses. A future-dated or zero age yields 1.
func gaussianDecay(age time.Duration, halflifeDays float64) float64 {
	days := age.Hours() / 24.0
	if days <= 0 {
		return 1
	}
	return math.Exp(-(days * days) / (2.0 * halflifeDays * halflifeDays))
}
