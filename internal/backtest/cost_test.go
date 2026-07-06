package backtest

import (
	"math"
	"testing"

	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

func TestCostPerTradeR(t *testing.T) {
	c := Costs{FeeRate: 0.0005, SlipRate: 0} // 0.05% per side

	// entry≈exit=100, stopDist=1 → cost = 0.0005*(100+100)/1 = 0.1R
	tr := &strategy.Trade{EntryPrice: 100, ExitPrice: 100, StopDist: 1}
	if got := c.PerTradeR(tr); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("PerTradeR=%v, want 0.1", got)
	}

	// A wider stop dilutes the cost: same prices, stopDist 4 → 0.025R.
	tr2 := &strategy.Trade{EntryPrice: 100, ExitPrice: 100, StopDist: 4}
	if got := c.PerTradeR(tr2); math.Abs(got-0.025) > 1e-9 {
		t.Errorf("PerTradeR(wide)=%v, want 0.025", got)
	}
}

func TestCostApplyReducesR(t *testing.T) {
	c := Costs{FeeRate: 0.0005, SlipRate: 0.0002} // 0.07% per side
	gross := []*strategy.Trade{
		{R: 2, EntryPrice: 100, ExitPrice: 98, StopDist: 1},
		{R: -1, EntryPrice: 100, ExitPrice: 101, StopDist: 1},
	}
	net := c.Apply(gross)

	if gross[0].R != 2 || gross[1].R != -1 {
		t.Fatal("Apply must not mutate the originals")
	}
	// cost0 = 0.0007*(100+98)/1 = 0.1386 → net 1.8614
	if math.Abs(net[0].R-(2-0.0007*198)) > 1e-9 {
		t.Errorf("net[0].R=%v", net[0].R)
	}
	if net[1].R >= gross[1].R { // a loss gets more negative
		t.Errorf("net loss should exceed gross: net=%v gross=%v", net[1].R, gross[1].R)
	}
}

func TestCostZeroIsNoOp(t *testing.T) {
	var c Costs
	in := []*strategy.Trade{{R: 1, EntryPrice: 10, ExitPrice: 10, StopDist: 1}}
	if got := c.Apply(in); &got[0] != &in[0] && got[0].R != 1 {
		t.Errorf("zero costs should pass trades through unchanged")
	}
}
