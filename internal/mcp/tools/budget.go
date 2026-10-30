package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/mo-cmyk/mazarbul/internal/kernel/stats"
	"github.com/mo-cmyk/mazarbul/internal/mcp"
)

// BudgetStore is the persistence surface for the user's monthly budget.
// The current implementation is in-memory; a persistence-backed wrapper
// could add cross-restart durability later.
type BudgetStore interface {
	Get(ctx context.Context) (stats.Budget, error)
	Set(ctx context.Context, b stats.Budget) error
}

// InMemoryBudgetStore is the default BudgetStore. Safe for concurrent
// use; intentionally not persisted — the user sets the budget each
// session for now, or wraps with a bolt store later.
type InMemoryBudgetStore struct {
	mu sync.RWMutex
	b  stats.Budget
}

// NewInMemoryBudgetStore returns an empty budget store.
func NewInMemoryBudgetStore() *InMemoryBudgetStore { return &InMemoryBudgetStore{} }

// Get implements BudgetStore.
func (s *InMemoryBudgetStore) Get(_ context.Context) (stats.Budget, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.b, nil
}

// Set implements BudgetStore.
func (s *InMemoryBudgetStore) Set(_ context.Context, b stats.Budget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = b
	return nil
}

// ---------- getBudgetStatus ----------

const budgetStatusInputSchema = `{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`

const budgetStatusOutputSchema = `{
  "type": "object",
  "properties": {
    "monthlyUsd":   {"type": "number", "minimum": 0},
    "spentMtdUsd":  {"type": "number", "minimum": 0},
    "remainingUsd": {"type": "number"},
    "alertAtPct":   {"type": "integer", "minimum": 0, "maximum": 100},
    "hitAlert":     {"type": "boolean"},
    "exhausted":    {"type": "boolean"}
  },
  "additionalProperties": false
}`

type budgetStatusOutput struct {
	MonthlyUSD   float64 `json:"monthlyUsd"`
	SpentMTDUSD  float64 `json:"spentMtdUsd"`
	RemainingUSD float64 `json:"remainingUsd"`
	AlertAtPct   int     `json:"alertAtPct"`
	HitAlert     bool    `json:"hitAlert"`
	Exhausted    bool    `json:"exhausted"`
}

// GetBudgetStatus is the MCP tool returning the current budget +
// month-to-date spend snapshot.
type GetBudgetStatus struct {
	store  BudgetStore
	stats  stats.Store
	prices []stats.Price
}

// NewGetBudgetStatus constructs the tool.
func NewGetBudgetStatus(s BudgetStore, st stats.Store, prices []stats.Price) *GetBudgetStatus {
	if len(prices) == 0 {
		prices = stats.DefaultPrices()
	}
	return &GetBudgetStatus{store: s, stats: st, prices: prices}
}

// Name implements mcp.Tool.
func (*GetBudgetStatus) Name() string { return "getBudgetStatus" }

// Description implements mcp.Tool.
func (*GetBudgetStatus) Description() string {
	return "Return the configured monthly budget, the month-to-date spend, the remaining headroom, and the alert state."
}

// InputSchema implements mcp.Tool.
func (*GetBudgetStatus) InputSchema() json.RawMessage {
	return json.RawMessage(budgetStatusInputSchema)
}

// OutputSchema implements mcp.Tool.
func (*GetBudgetStatus) OutputSchema() json.RawMessage {
	return json.RawMessage(budgetStatusOutputSchema)
}

// Invoke implements mcp.Tool.
func (g *GetBudgetStatus) Invoke(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if g.store == nil {
		return nil, mcp.NewInternalError(errors.New("budget store not wired"))
	}
	budget, err := g.store.Get(ctx)
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("budget get: %w", err))
	}
	var spent float64
	if g.stats != nil {
		snap, snapErr := g.stats.Snapshot(ctx, stats.Last30Days)
		if snapErr == nil {
			cb := stats.ComputeCost(snap, g.prices, "monthToDate")
			spent = cb.TotalUSD
		}
	}
	st := budget.Evaluate(spent)
	body, err := json.Marshal(budgetStatusOutput{
		MonthlyUSD:   st.MonthlyUSD,
		SpentMTDUSD:  st.SpentMTDUSD,
		RemainingUSD: st.RemainingUSD,
		AlertAtPct:   st.AlertAtPct,
		HitAlert:     st.HitAlert,
		Exhausted:    st.Exhausted,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}

// ---------- setBudget ----------

const setBudgetInputSchema = `{
  "type": "object",
  "properties": {
    "monthlyUsd": {"type": "number", "minimum": 0, "description": "Monthly cap in USD. Set to 0 to clear the budget."},
    "alertAtPct": {"type": "integer", "minimum": 0, "maximum": 100, "description": "Alert threshold as percent of the monthly cap. Defaults to 80 when omitted."}
  },
  "required": ["monthlyUsd"],
  "additionalProperties": false
}`

const setBudgetOutputSchema = `{
  "type": "object",
  "properties": {
    "monthlyUsd": {"type": "number", "minimum": 0},
    "alertAtPct": {"type": "integer", "minimum": 0, "maximum": 100}
  },
  "required": ["monthlyUsd", "alertAtPct"],
  "additionalProperties": false
}`

type setBudgetInput struct {
	MonthlyUSD float64 `json:"monthlyUsd"`
	AlertAtPct int     `json:"alertAtPct,omitempty"`
}

type setBudgetOutput struct {
	MonthlyUSD float64 `json:"monthlyUsd"`
	AlertAtPct int     `json:"alertAtPct"`
}

// SetBudget is the MCP tool that updates the monthly budget.
type SetBudget struct{ store BudgetStore }

// NewSetBudget constructs the tool.
func NewSetBudget(s BudgetStore) *SetBudget { return &SetBudget{store: s} }

// Name implements mcp.Tool.
func (*SetBudget) Name() string { return "setBudget" }

// Description implements mcp.Tool.
func (*SetBudget) Description() string {
	return "Update the monthly USD budget and (optionally) the alert threshold percent."
}

// InputSchema implements mcp.Tool.
func (*SetBudget) InputSchema() json.RawMessage { return json.RawMessage(setBudgetInputSchema) }

// OutputSchema implements mcp.Tool.
func (*SetBudget) OutputSchema() json.RawMessage { return json.RawMessage(setBudgetOutputSchema) }

// Invoke implements mcp.Tool.
func (s *SetBudget) Invoke(ctx context.Context, req json.RawMessage) (json.RawMessage, error) {
	if s.store == nil {
		return nil, mcp.NewInternalError(errors.New("budget store not wired"))
	}
	var in setBudgetInput
	if err := json.Unmarshal(req, &in); err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("decode arguments: %w", err))
	}
	if in.AlertAtPct == 0 {
		in.AlertAtPct = 80
	}
	b := stats.Budget{MonthlyUSD: in.MonthlyUSD, AlertAtPct: in.AlertAtPct}
	if err := s.store.Set(ctx, b); err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("budget set: %w", err))
	}
	body, err := json.Marshal(setBudgetOutput{
		MonthlyUSD: b.MonthlyUSD,
		AlertAtPct: b.AlertAtPct,
	})
	if err != nil {
		return nil, mcp.NewInternalError(fmt.Errorf("marshal output: %w", err))
	}
	return body, nil
}
