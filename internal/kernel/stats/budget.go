package stats

// Budget configures the monthly spend cap displayed in the Cost panel
// and the doctor budget check. It is configuration-only — there is
// no enforcement (hard cap) yet.
type Budget struct {
	MonthlyUSD float64 `toml:"monthlyUsd"`
	AlertAtPct int     `toml:"alertAtPct"`
}

// BudgetStatus reports the current state of a Budget given the
// month-to-date spend. UnderAlert/HitAlert are booleans the doctor
// check + Cost panel both consume.
type BudgetStatus struct {
	MonthlyUSD   float64
	SpentMTDUSD  float64
	RemainingUSD float64
	AlertAtPct   int
	HitAlert     bool
	Exhausted    bool
}

// Evaluate returns the status of the budget given spend so far.
func (b Budget) Evaluate(spentUSD float64) BudgetStatus {
	st := BudgetStatus{
		MonthlyUSD:   b.MonthlyUSD,
		SpentMTDUSD:  spentUSD,
		RemainingUSD: b.MonthlyUSD - spentUSD,
		AlertAtPct:   b.AlertAtPct,
	}
	if b.MonthlyUSD <= 0 {
		return st
	}
	alertPct := b.AlertAtPct
	if alertPct <= 0 {
		alertPct = 80
	}
	pct := spentUSD / b.MonthlyUSD * 100
	st.HitAlert = pct >= float64(alertPct)
	st.Exhausted = spentUSD >= b.MonthlyUSD
	return st
}
