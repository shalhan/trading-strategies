package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tz database unavailable: %v", err)
	}
	return loc
}

// cndl builds a 5m candle opening at the given NY wall-clock time.
func cndl(loc *time.Location, day, hour, min int, o, h, l, c float64) kline.Kline {
	ot := time.Date(2026, time.January, day, hour, min, 0, 0, loc)
	return kline.Kline{
		OpenTime: ot, Open: o, High: h, Low: l, Close: c,
		CloseTime: ot.Add(5 * time.Minute),
	}
}

// baseCfg: ATR ready after one candle, filter effectively off, cap 2.
func baseCfg(loc *time.Location) Config {
	return Config{
		Symbol: "TESTUSDT", Loc: loc,
		ATRPeriod: 1, MaxStopATR: 100, MaxAttemptsPerSide: 2,
		StopBufferTicks: 0, TickSize: 0.01,
	}
}

// run feeds candles, auto-accepting every proposal, and collects completed trades.
func run(e *Engine, ks []kline.Kline) []*Trade {
	var trades []*Trade
	for _, k := range ks {
		res := e.Step(k)
		if res.Proposal != nil {
			e.Resolve(true)
		}
		if res.Closed != nil {
			trades = append(trades, res.Closed)
		}
	}
	return trades
}

// window establishes a 90–110 range with the 00:00 candle present (full window).
func window(loc *time.Location, day int) []kline.Kline {
	return []kline.Kline{
		cndl(loc, day, 0, 0, 100, 110, 90, 105), // sets range high 110 / low 90
		cndl(loc, day, 0, 5, 105, 108, 95, 100),
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestShortHitsTarget(t *testing.T) {
	loc := nyLoc(t)
	e := New(baseCfg(loc))

	ks := window(loc, 2)
	ks = append(ks,
		cndl(loc, 2, 4, 0, 100, 112, 99, 111), // close 111 > 110 → break above
		cndl(loc, 2, 4, 5, 111, 115, 108, 109), // close back below 110 → enter short @109, extreme 115, stop 115, target 97
		cndl(loc, 2, 4, 10, 109, 110, 96, 98),  // low 96 ≤ 97 → target
	)
	trades := run(e, ks)

	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Side != Short || tr.Outcome != OutcomeTarget {
		t.Fatalf("got %s %s, want SHORT target", tr.Side, tr.Outcome)
	}
	if !approx(tr.EntryPrice, 109) || !approx(tr.Stop, 115) || !approx(tr.ExitPrice, 97) {
		t.Errorf("prices entry=%v stop=%v exit=%v, want 109/115/97", tr.EntryPrice, tr.Stop, tr.ExitPrice)
	}
	if !approx(tr.R, 2) {
		t.Errorf("R=%v, want 2", tr.R)
	}
}

func TestShortHitsStop(t *testing.T) {
	loc := nyLoc(t)
	e := New(baseCfg(loc))

	ks := window(loc, 2)
	ks = append(ks,
		cndl(loc, 2, 4, 0, 100, 112, 99, 111),
		cndl(loc, 2, 4, 5, 111, 115, 108, 109), // short @109, stop 115
		cndl(loc, 2, 4, 10, 109, 116, 108, 114), // high 116 ≥ 115 → stop
	)
	trades := run(e, ks)
	if len(trades) != 1 || trades[0].Outcome != OutcomeStop {
		t.Fatalf("want 1 stop trade, got %+v", trades)
	}
	if !approx(trades[0].R, -1) {
		t.Errorf("R=%v, want -1", trades[0].R)
	}
}

func TestLongHitsTarget(t *testing.T) {
	loc := nyLoc(t)
	e := New(baseCfg(loc))

	ks := window(loc, 2)
	ks = append(ks,
		cndl(loc, 2, 4, 0, 100, 101, 88, 89),   // close 89 < 90 → break below
		cndl(loc, 2, 4, 5, 89, 92, 85, 91),     // close back above 90 → enter long @91, extreme 85, stop 85, target 103
		cndl(loc, 2, 4, 10, 91, 104, 90, 102),  // high 104 ≥ 103 → target
	)
	trades := run(e, ks)
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Side != Long || tr.Outcome != OutcomeTarget {
		t.Fatalf("got %s %s, want LONG target", tr.Side, tr.Outcome)
	}
	if !approx(tr.EntryPrice, 91) || !approx(tr.Stop, 85) || !approx(tr.R, 2) {
		t.Errorf("entry=%v stop=%v R=%v, want 91/85/2", tr.EntryPrice, tr.Stop, tr.R)
	}
}

func TestMaxStopDistanceFilterSkips(t *testing.T) {
	loc := nyLoc(t)
	cfg := baseCfg(loc)
	cfg.MaxStopATR = 0.1 // ATR≈7 → max dist ≈0.7, well below the ~6 stop distance
	e := New(cfg)

	ks := window(loc, 2)
	ks = append(ks,
		cndl(loc, 2, 4, 0, 100, 112, 99, 111),
		cndl(loc, 2, 4, 5, 111, 115, 108, 109), // would enter, but stop dist ~6 > filter → skip
		cndl(loc, 2, 4, 10, 109, 110, 96, 98),
	)
	trades := run(e, ks)
	if len(trades) != 0 {
		t.Fatalf("filter should skip the trade, got %+v", trades)
	}
	if e.State() != Watching {
		t.Errorf("state=%s, want WATCHING after skip", e.State())
	}
}

func TestAttemptsCapPerSide(t *testing.T) {
	loc := nyLoc(t)
	cfg := baseCfg(loc)
	cfg.MaxAttemptsPerSide = 1
	e := New(cfg)

	ks := window(loc, 2)
	ks = append(ks,
		// first short: break, reentry, target
		cndl(loc, 2, 4, 0, 100, 112, 99, 111),
		cndl(loc, 2, 4, 5, 111, 115, 108, 109),
		cndl(loc, 2, 4, 10, 109, 110, 96, 98), // target → re-arm
		// second short break should be ignored (cap reached)
		cndl(loc, 2, 4, 15, 100, 113, 99, 112),
		cndl(loc, 2, 4, 20, 112, 113, 105, 106),
	)
	trades := run(e, ks)
	if len(trades) != 1 {
		t.Fatalf("attempts cap: want 1 trade, got %d", len(trades))
	}
}

func TestForceCloseAtNYMidnight(t *testing.T) {
	loc := nyLoc(t)
	e := New(baseCfg(loc))

	ks := window(loc, 2)
	ks = append(ks,
		cndl(loc, 2, 4, 0, 100, 112, 99, 111),
		cndl(loc, 2, 4, 5, 111, 115, 108, 109),   // short @109, stop 115, target 97
		cndl(loc, 2, 4, 10, 109, 111, 100, 105),  // neither hit → still in position into EOD
		cndl(loc, 3, 0, 0, 104, 106, 102, 105),   // new NY day → force-close at open 104
	)
	trades := run(e, ks)
	if len(trades) != 1 || trades[0].Outcome != OutcomeEOD {
		t.Fatalf("want 1 EOD trade, got %+v", trades)
	}
	if !approx(trades[0].ExitPrice, 104) {
		t.Errorf("EOD exit=%v, want 104 (next-day open)", trades[0].ExitPrice)
	}
	if e.State() != WaitRange {
		t.Errorf("state=%s, want WAIT_RANGE after rollover", e.State())
	}
}

func TestIncompleteWindowNotTradable(t *testing.T) {
	loc := nyLoc(t)
	e := New(baseCfg(loc))

	// Data starts at 02:00 ET — the 00:00 candle was never seen.
	ks := []kline.Kline{
		cndl(loc, 2, 2, 0, 100, 110, 90, 105),
		cndl(loc, 2, 4, 0, 105, 112, 99, 111), // would be a break, but range not trustworthy
		cndl(loc, 2, 4, 5, 111, 115, 108, 109),
	}
	trades := run(e, ks)
	if len(trades) != 0 {
		t.Fatalf("incomplete window must not trade, got %+v", trades)
	}
	if e.rangeReady {
		t.Errorf("rangeReady should be false on incomplete window")
	}
}

func TestTrailingStopLetsWinnerRunPast2R(t *testing.T) {
	loc := nyLoc(t)
	cfg := baseCfg(loc)
	cfg.TrailATR = 1 // trail by 1×ATR (ATRPeriod=1 ⇒ ATR≈last TR)

	// Short entry at 109, stop 115 (extreme), 1R≈6. Price then falls hard, far
	// past the old 2R target (97), and the trailing stop exits at a big win.
	ks := window(loc, 2)
	ks = append(ks,
		cndl(loc, 2, 4, 0, 100, 112, 99, 111),  // break above
		cndl(loc, 2, 4, 5, 111, 115, 108, 109), // reentry → short @109, extreme 115
		cndl(loc, 2, 4, 10, 109, 110, 90, 92),  // big drop; would blow past 2R target (97)
		cndl(loc, 2, 4, 15, 92, 80, 70, 72),    // keeps falling
		cndl(loc, 2, 4, 20, 72, 95, 71, 94),    // snaps back up → trailing stop hit
	)
	trades := run(e2(cfg), ks)
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Outcome != OutcomeTrail {
		t.Errorf("outcome=%s, want trail", tr.Outcome)
	}
	if tr.R <= 2 {
		t.Errorf("trailing winner R=%.2f, want > 2 (ran past the old 2R cap)", tr.R)
	}
}

// e2 builds an engine (helper to keep the test terse).
func e2(cfg Config) *Engine { return New(cfg) }

func TestTrendFilterSkipsCounterTrendShort(t *testing.T) {
	loc := nyLoc(t)

	// Rising closes ⇒ fastEMA > slowEMA ⇒ uptrend. A short (fading a break above)
	// fights that trend and must be skipped when the filter is on.
	ks := []kline.Kline{
		cndl(loc, 2, 0, 0, 100, 109, 99, 101),  // 00:00 present; range high 109 via wick
		cndl(loc, 2, 0, 5, 101, 105, 100, 103), // closes rising: 101, 103
		cndl(loc, 2, 4, 0, 103, 112, 103, 110), // break above 109; close 110 (rising)
		cndl(loc, 2, 4, 5, 110, 111, 106, 108), // close 108 < 109 → reentry (short setup)
	}

	cfgOff := baseCfg(loc) // no trend filter
	cfgOn := baseCfg(loc)
	cfgOn.TrendFastEMA, cfgOn.TrendSlowEMA, cfgOn.TrendThreshold = 2, 3, 0 // strict: any uptrend blocks

	// Filter off: the reentry produces a proposal.
	if !sawProposalAtReentry(New(cfgOff), ks) {
		t.Fatal("filter off: expected a short proposal on reentry")
	}
	// Filter on: the counter-trend short is skipped.
	if sawProposalAtReentry(New(cfgOn), ks) {
		t.Fatal("filter on: counter-trend short should be skipped")
	}
}

// sawProposalAtReentry steps the candles and reports whether any proposal was
// produced (auto-rejecting so the engine stays clean).
func sawProposalAtReentry(e *Engine, ks []kline.Kline) bool {
	saw := false
	for _, k := range ks {
		if res := e.Step(k); res.Proposal != nil {
			saw = true
			e.Resolve(false)
		}
	}
	return saw
}

func TestProposeAndResolve(t *testing.T) {
	loc := nyLoc(t)

	// Rejecting the proposal must prevent entry and re-arm to WATCHING.
	t.Run("reject", func(t *testing.T) {
		e := New(baseCfg(loc))
		ks := window(loc, 2)
		ks = append(ks,
			cndl(loc, 2, 4, 0, 100, 112, 99, 111),
			cndl(loc, 2, 4, 5, 111, 115, 108, 109), // reentry → proposal
		)
		var sawProposal bool
		for _, k := range ks {
			res := e.Step(k)
			if res.Proposal != nil {
				sawProposal = true
				if res.Proposal.Side != Short {
					t.Errorf("proposal side=%s, want SHORT", res.Proposal.Side)
				}
				e.Resolve(false) // portfolio rejects
			}
		}
		if !sawProposal {
			t.Fatal("expected a proposal on reentry")
		}
		if e.State() != Watching {
			t.Errorf("state=%s, want WATCHING after reject", e.State())
		}
		// A following target-bound candle must NOT produce a trade.
		if res := e.Step(cndl(loc, 2, 4, 10, 109, 110, 96, 98)); res.Closed != nil {
			t.Errorf("no position should exist after reject, got %+v", res.Closed)
		}
	})

	// Accepting reproduces the normal short-to-target trade.
	t.Run("accept", func(t *testing.T) {
		e := New(baseCfg(loc))
		ks := window(loc, 2)
		ks = append(ks,
			cndl(loc, 2, 4, 0, 100, 112, 99, 111),
			cndl(loc, 2, 4, 5, 111, 115, 108, 109),
			cndl(loc, 2, 4, 10, 109, 110, 96, 98),
		)
		trades := run(e, ks)
		if len(trades) != 1 || trades[0].Outcome != OutcomeTarget {
			t.Fatalf("want 1 target trade on accept, got %+v", trades)
		}
	})
}
