package backtest

import (
	"math"
	"testing"

	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

func TestComputeStats(t *testing.T) {
	trades := []*strategy.Trade{
		{R: 2, Outcome: strategy.OutcomeTarget},
		{R: -1, Outcome: strategy.OutcomeStop},
		{R: -1, Outcome: strategy.OutcomeStop},
		{R: 2, Outcome: strategy.OutcomeTarget},
	}
	s := Compute(trades, 10000, 0.01) // risk $100/trade

	if s.Trades != 4 || s.Wins != 2 || s.Losses != 2 {
		t.Fatalf("counts wrong: %+v", s)
	}
	if s.Targets != 2 || s.Stops != 2 {
		t.Errorf("outcome counts wrong: targets=%d stops=%d", s.Targets, s.Stops)
	}
	if math.Abs(s.TotalR-2) > 1e-9 { // 2 -1 -1 2
		t.Errorf("TotalR=%v, want 2", s.TotalR)
	}
	if math.Abs(s.ExpectancyR-0.5) > 1e-9 {
		t.Errorf("ExpectancyR=%v, want 0.5", s.ExpectancyR)
	}
	if math.Abs(s.ProfitFactor-2) > 1e-9 { // 4 / 2
		t.Errorf("ProfitFactor=%v, want 2", s.ProfitFactor)
	}
	// Equity curve: +200, +100, 0, +200 → end 10200. Max DD in R: peak 2 (after t1),
	// trough 0 (after t3) → 2R.
	if math.Abs(s.MaxDrawdownR-2) > 1e-9 {
		t.Errorf("MaxDrawdownR=%v, want 2", s.MaxDrawdownR)
	}
	if math.Abs(s.EndEquity-10200) > 1e-9 {
		t.Errorf("EndEquity=%v, want 10200", s.EndEquity)
	}
}

func TestComputeProfitFactorNoLosses(t *testing.T) {
	s := Compute([]*strategy.Trade{{R: 2, Outcome: strategy.OutcomeTarget}}, 0, 0)
	if !math.IsInf(s.ProfitFactor, 1) {
		t.Errorf("ProfitFactor=%v, want +Inf with no losses", s.ProfitFactor)
	}
}
