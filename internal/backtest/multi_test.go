package backtest

import (
	"testing"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/portfolio"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	return loc
}

func cndl(loc *time.Location, hour, min int, o, h, l, c float64) kline.Kline {
	ot := time.Date(2026, time.January, 2, hour, min, 0, 0, loc)
	return kline.Kline{OpenTime: ot, Open: o, High: h, Low: l, Close: c, CloseTime: ot.Add(5 * time.Minute)}
}

func engineFor(sym string, loc *time.Location) *strategy.Engine {
	return strategy.New(strategy.Config{
		Symbol: sym, Loc: loc, ATRPeriod: 1, MaxStopATR: 100,
		MaxAttemptsPerSide: 2, StopBufferTicks: 0, TickSize: 0.01,
	})
}

// twoSymbolBar builds two symbols that both reentry-short on the same 04:05 bar,
// one with a tight stop, one with a wide stop.
func twoSymbolBar(loc *time.Location) map[string][]kline.Kline {
	window := []kline.Kline{
		cndl(loc, 0, 0, 100, 110, 90, 105), // range 90–110, 00:00 present
		cndl(loc, 0, 5, 105, 108, 95, 100),
	}
	tight := append(append([]kline.Kline{}, window...),
		cndl(loc, 4, 0, 100, 112, 99, 111),  // break above
		cndl(loc, 4, 5, 111, 113, 108, 109), // reentry: extreme 113, stop dist ~4 (tight)
		cndl(loc, 4, 10, 109, 110, 100, 101), // → target
	)
	wide := append(append([]kline.Kline{}, window...),
		cndl(loc, 4, 0, 100, 120, 99, 111),  // break above, far extreme
		cndl(loc, 4, 5, 111, 122, 108, 109), // reentry: extreme 122, stop dist ~13 (wide)
		cndl(loc, 4, 10, 109, 110, 100, 101),
	)
	return map[string][]kline.Kline{"TIGHTUSDT": tight, "WIDEUSDT": wide}
}

func TestPortfolioTakesTighterUnderCap(t *testing.T) {
	loc := nyLoc(t)
	series := twoSymbolBar(loc)
	data := map[string]SymbolData{
		"TIGHTUSDT": {Engine: engineFor("TIGHTUSDT", loc), Klines: series["TIGHTUSDT"]},
		"WIDEUSDT":  {Engine: engineFor("WIDEUSDT", loc), Klines: series["WIDEUSDT"]},
	}
	// Only one slot: the tighter-stop signal must win, the wider is skipped.
	mgr := portfolio.New(portfolio.Config{
		Capital: 10000, RiskPerTrade: 0.01, MaxConcurrent: 1, MaxTotalRisk: 0.05,
	})
	trades := RunPortfolio(data, mgr)

	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1 (cap=1)", len(trades))
	}
	if trades[0].Symbol != "TIGHTUSDT" {
		t.Errorf("admitted %s, want TIGHTUSDT (tighter stop)", trades[0].Symbol)
	}
	if trades[0].Outcome != strategy.OutcomeTarget {
		t.Errorf("outcome=%s, want target", trades[0].Outcome)
	}
}

func TestPortfolioAdmitsBothWhenRoom(t *testing.T) {
	loc := nyLoc(t)
	series := twoSymbolBar(loc)
	tight := engineFor("TIGHTUSDT", loc)
	wide := engineFor("WIDEUSDT", loc)
	data := map[string]SymbolData{
		"TIGHTUSDT": {Engine: tight, Klines: series["TIGHTUSDT"]},
		"WIDEUSDT":  {Engine: wide, Klines: series["WIDEUSDT"]},
	}
	mgr := portfolio.New(portfolio.Config{
		Capital: 10000, RiskPerTrade: 0.01, MaxConcurrent: 5, MaxTotalRisk: 0.05,
	})
	RunPortfolio(data, mgr)

	// Both entered this bar; TIGHT reached target and freed its slot. Verify
	// neither engine is stuck in PENDING_ENTRY (every proposal was resolved).
	for _, e := range []*strategy.Engine{tight, wide} {
		if e.State() == strategy.PendingEntry {
			t.Errorf("%s stuck in PENDING_ENTRY (proposal not resolved)", e.Symbol())
		}
	}
}
