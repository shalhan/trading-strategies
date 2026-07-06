// Package strategy implements the per-symbol failed-break state machine:
//
//	WAIT_RANGE → WATCHING → BROKEN → PENDING_ENTRY → IN_POSITION → FLAT → (re-arm)
//
// One Engine drives one symbol. Entries are confirmation-close (the reentry
// candle's close) so backtest and live agree. The Engine is capital-agnostic:
// it emits trades with prices and R-multiples; account sizing/PnL and
// cross-symbol risk live in the portfolio/backtest layer.
//
// Entry is two-phase so a portfolio can rank competing signals before
// committing capital: when a reentry passes the ATR filter, Step returns a
// Proposal and the engine parks in PENDING_ENTRY; the caller then calls Resolve
// to either commit the position or stand down. Single-symbol callers simply
// Resolve(true) immediately.
package strategy

import (
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/indicator"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

// State is the per-symbol machine state.
type State int

const (
	WaitRange    State = iota // before the 4H window closes; no levels yet
	Watching                  // range set; watching for a 5m close beyond it
	Broken                    // a level broke; tracking the false-break extreme
	PendingEntry              // reentry passed the filter; awaiting Resolve
	InPosition                // entered at reentry close; managing stop and target
)

func (s State) String() string {
	switch s {
	case WaitRange:
		return "WAIT_RANGE"
	case Watching:
		return "WATCHING"
	case Broken:
		return "BROKEN"
	case PendingEntry:
		return "PENDING_ENTRY"
	case InPosition:
		return "IN_POSITION"
	}
	return "UNKNOWN"
}

// Side is the trade direction. A break above the range high is faded Short; a
// break below the low is faded Long.
type Side int

const (
	Long Side = iota
	Short
)

func (s Side) String() string {
	if s == Short {
		return "SHORT"
	}
	return "LONG"
}

// Outcome is how a trade closed.
type Outcome string

const (
	OutcomeTarget Outcome = "target"
	OutcomeStop   Outcome = "stop"
	OutcomeEOD    Outcome = "eod"   // force-closed at NY midnight
	OutcomeTrail  Outcome = "trail" // trailing stop hit (may be a win or loss)
)

// Config is the per-symbol tuning. RISK_PER_TRADE and capital are deliberately
// absent — those belong to the portfolio/backtest layer.
type Config struct {
	Symbol             string
	Loc                *time.Location
	ATRPeriod          int     // Wilder ATR period for the stop filter (default 14)
	MaxStopATR         float64 // skip if stop distance > MaxStopATR × ATR; MUST be tuned
	MaxAttemptsPerSide int     // entries per side per NY day (default 2)
	StopBufferTicks    float64 // buffer beyond the extreme, in ticks (default a few)
	TickSize           float64 // price tick size for the symbol

	// HoldOvernight, when true, disables the NY-midnight force-close: a position
	// runs until its stop or 2R target, even across days. The spec default is
	// to force-close (same-day only), so the zero value preserves that. While a
	// position is held across midnight, that symbol takes no new setups and the
	// day it rolls into is untradable for new entries (its opening window is
	// missed) — a deliberate cost of holding.
	HoldOvernight bool

	// Trend filter (mean-reversion protection): skip a fade that fights a strong
	// trend — don't short a failed break-up in an uptrend, nor long a failed
	// break-down in a downtrend. Trend = (fastEMA − slowEMA) / ATR, so it is
	// volatility-normalized and comparable across symbols. The filter is active
	// only when both EMA periods are > 0; the zero value disables it.
	TrendFastEMA   int     // fast EMA period on closes (e.g. 20)
	TrendSlowEMA   int     // slow EMA period on closes (e.g. 50)
	TrendThreshold float64 // skip when trend strength exceeds this (in ATR units), in the break's direction

	// TrendSkipWith inverts the filter: skip fades that align WITH the trend
	// instead of against it (keep only counter-trend / exhaustion fades).
	// Empirically the with-trend fades are the weaker ones for this strategy.
	TrendSkipWith bool

	// TrailATR, when > 0, replaces the fixed 2R target with an ATR trailing
	// stop: the position has no profit cap and exits when price retraces
	// TrailATR × ATR(at entry) from its best favorable level. Lets winners run
	// past 2R (raising average winner size) at the cost of giving some back. The
	// zero value keeps the fixed 2R target.
	TrailATR float64
}

func (c *Config) withDefaults() {
	if c.ATRPeriod <= 0 {
		c.ATRPeriod = 14
	}
	if c.MaxAttemptsPerSide <= 0 {
		c.MaxAttemptsPerSide = 2
	}
}

// Proposal is an entry the Engine is ready to take, pending portfolio approval.
type Proposal struct {
	Symbol   string
	Side     Side
	Time     time.Time
	Entry    float64
	Stop     float64
	Target   float64
	StopDist float64 // abs(entry - stop) = 1R in price
}

// Trade is a completed round-trip, ready for the trade log and stats.
type Trade struct {
	Symbol     string
	Side       Side
	EntryTime  time.Time
	EntryPrice float64
	Stop       float64
	Target     float64
	StopDist   float64
	ExitTime   time.Time
	ExitPrice  float64
	Outcome    Outcome
	R          float64 // realized R multiple (+2 target, -1 stop, fractional on EOD)
	ATRAtEntry float64
	Setup      string  // optional tag (e.g. "BOS"/"CHoCH"); empty for the failed-break engine
	MakerEntry bool    // true if entered via a resting limit order (maker fee, no entry slippage)
	Trigger    float64 // break-even trigger level (the prev swing high/low); 0 if unused
	BEPArmed   bool    // true if the break-even stop was armed (price reached Trigger)
}

// OpenPosition is a read-only snapshot of a live position for the portfolio
// layer's risk accounting.
type OpenPosition struct {
	Symbol   string
	Side     Side
	Entry    float64
	Stop     float64
	StopDist float64
}

// StepResult is what one candle produced. At most one field is non-nil:
// Closed (a trade finished) xor Proposal (an entry awaits Resolve).
type StepResult struct {
	Closed   *Trade
	Proposal *Proposal
}

// Engine is the per-symbol state machine.
type Engine struct {
	cfg Config
	atr *indicator.ATR

	// trend filter indicators; nil when the filter is disabled
	emaFast *indicator.EMA
	emaSlow *indicator.EMA
	fastVal float64
	slowVal float64
	emaReady bool

	state State

	// per-NY-day state, reset on rollover
	day            string
	windowComplete bool // saw the 00:00 ET candle, so the range is trustworthy
	collectedAny   bool // collected at least one window candle
	rangeReady     bool // range usable (window closed and complete)
	rangeHigh      float64
	rangeLow       float64
	attemptsLong   int
	attemptsShort  int

	// BROKEN-state tracking
	setupSide Side    // Short = broke above, Long = broke below
	extreme   float64 // false-break extreme

	pending *pendingEntry // set in PENDING_ENTRY, consumed by Resolve
	pos     *position     // live position
}

type pendingEntry struct {
	side     Side
	time     time.Time
	entry    float64
	stop     float64
	target   float64
	stopDist float64
	atr      float64
}

type position struct {
	side       Side
	entryTime  time.Time
	entry      float64
	stop       float64
	target     float64
	stopDist   float64
	atrAtEntry float64
	bestLow    float64 // lowest low since entry (for trailing a short)
	bestHigh   float64 // highest high since entry (for trailing a long)
}

// New builds an Engine for one symbol.
func New(cfg Config) *Engine {
	cfg.withDefaults()
	e := &Engine{
		cfg:   cfg,
		atr:   indicator.NewATR(cfg.ATRPeriod),
		state: WaitRange,
	}
	if cfg.TrendFastEMA > 0 && cfg.TrendSlowEMA > 0 {
		e.emaFast = indicator.NewEMA(cfg.TrendFastEMA)
		e.emaSlow = indicator.NewEMA(cfg.TrendSlowEMA)
	}
	return e
}

// trendFilterOn reports whether the trend filter is active.
func (e *Engine) trendFilterOn() bool { return e.emaFast != nil }

// State returns the current machine state (for diagnostics/tests).
func (e *Engine) State() State { return e.state }

// Symbol returns the engine's symbol.
func (e *Engine) Symbol() string { return e.cfg.Symbol }

// OpenPosition returns a snapshot of the live position, or nil if flat.
func (e *Engine) OpenPosition() *OpenPosition {
	if e.pos == nil {
		return nil
	}
	return &OpenPosition{
		Symbol: e.cfg.Symbol, Side: e.pos.side,
		Entry: e.pos.entry, Stop: e.pos.stop, StopDist: e.pos.stopDist,
	}
}

// Step advances the machine by one closed 5m candle. It returns a Closed trade
// (a stop/target hit, or a force-close at the NY-midnight rollover) or a
// Proposal (a reentry that passed the ATR filter and now awaits Resolve), or
// neither. The caller must Resolve a Proposal before the next Step.
func (e *Engine) Step(k kline.Kline) StepResult {
	atrVal, atrReady := e.atr.Update(k)
	if e.trendFilterOn() {
		f, fr := e.emaFast.Update(k.Close)
		s, sr := e.emaSlow.Update(k.Close)
		e.fastVal, e.slowVal, e.emaReady = f, s, fr && sr
	}

	// NY-day rollover.
	if day := k.NYDate(e.cfg.Loc); day != e.day {
		// Hold mode: let an open position run past midnight to its stop/target.
		if e.pos != nil && e.cfg.HoldOvernight {
			e.day = day // advance the day marker so rollover doesn't re-fire each candle
			if tr := e.managePosition(k); tr != nil {
				// Closed during the new day: start fresh. Today is untradable
				// (its 00:00 window candle was missed while holding); the next
				// NY day re-arms normally.
				e.resetDay(day)
				return StepResult{Closed: tr}
			}
			return StepResult{} // still holding
		}
		// Default: force-close at midnight, then reset.
		var forced *Trade
		if e.pos != nil {
			forced = e.closePosition(k.Open, k.OpenTime, OutcomeEOD)
		}
		e.resetDay(day)
		e.collectWindow(k) // this candle is the first of the new day
		return StepResult{Closed: forced}
	}

	switch e.state {
	case WaitRange:
		e.handleWaitRange(k)
	case Watching:
		e.handleWatching(k)
	case Broken:
		if p := e.handleBroken(k, atrVal, atrReady); p != nil {
			return StepResult{Proposal: p}
		}
	case InPosition:
		if tr := e.managePosition(k); tr != nil {
			return StepResult{Closed: tr}
		}
	}
	return StepResult{}
}

// Resolve commits or abandons a pending entry. accept=true enters the position
// (consuming one attempt for that side); accept=false stands down and re-arms
// to WATCHING without consuming an attempt. It is a no-op unless the engine is
// in PENDING_ENTRY.
func (e *Engine) Resolve(accept bool) {
	if e.state != PendingEntry || e.pending == nil {
		return
	}
	pe := e.pending
	e.pending = nil
	if !accept {
		e.state = Watching
		return
	}
	e.pos = &position{
		side: pe.side, entryTime: pe.time, entry: pe.entry,
		stop: pe.stop, target: pe.target, stopDist: pe.stopDist, atrAtEntry: pe.atr,
		bestLow: pe.entry, bestHigh: pe.entry,
	}
	if pe.side == Short {
		e.attemptsShort++
	} else {
		e.attemptsLong++
	}
	e.state = InPosition
}

func (e *Engine) resetDay(day string) {
	e.day = day
	e.windowComplete = false
	e.collectedAny = false
	e.rangeReady = false
	e.rangeHigh = 0
	e.rangeLow = 0
	e.attemptsLong = 0
	e.attemptsShort = 0
	e.state = WaitRange
	e.pending = nil
	e.pos = nil
}

// collectWindow accumulates a window candle into the range, or closes the
// window and finalizes the range on the first candle past 04:00 ET.
func (e *Engine) collectWindow(k kline.Kline) {
	if k.InOpeningWindow(e.cfg.Loc) {
		if k.IsNYMidnightOpen(e.cfg.Loc) {
			e.windowComplete = true
		}
		if !e.collectedAny {
			e.rangeHigh, e.rangeLow = k.High, k.Low
			e.collectedAny = true
		} else {
			if k.High > e.rangeHigh {
				e.rangeHigh = k.High
			}
			if k.Low < e.rangeLow {
				e.rangeLow = k.Low
			}
		}
		return
	}
	// First candle past the window: finalize and start watching. The range is
	// only trustworthy if we saw the 00:00 candle (full window observed).
	e.rangeReady = e.windowComplete && e.collectedAny
	e.state = Watching
	if e.rangeReady {
		e.handleWatching(k) // the 04:00 candle can itself be a break
	}
}

func (e *Engine) handleWaitRange(k kline.Kline) {
	e.collectWindow(k)
}

// handleWatching looks for a confirmed break: a 5m close beyond the range,
// respecting the per-side attempt cap.
func (e *Engine) handleWatching(k kline.Kline) {
	if !e.rangeReady {
		return
	}
	switch {
	case k.Close > e.rangeHigh && e.attemptsShort < e.cfg.MaxAttemptsPerSide:
		e.setupSide = Short
		e.extreme = k.High
		e.state = Broken
	case k.Close < e.rangeLow && e.attemptsLong < e.cfg.MaxAttemptsPerSide:
		e.setupSide = Long
		e.extreme = k.Low
		e.state = Broken
	}
}

// handleBroken tracks the false-break extreme and, on a reentry close back
// inside the range, applies the ATR stop filter. On a pass it parks the engine
// in PENDING_ENTRY and returns the Proposal; a filtered setup returns to
// WATCHING without consuming an attempt.
func (e *Engine) handleBroken(k kline.Kline, atrVal float64, atrReady bool) *Proposal {
	if e.setupSide == Short {
		if k.High > e.extreme {
			e.extreme = k.High
		}
		if k.Close >= e.rangeHigh {
			return nil // still broken above; no reentry yet
		}
	} else {
		if k.Low < e.extreme {
			e.extreme = k.Low
		}
		if k.Close <= e.rangeLow {
			return nil // still broken below
		}
	}

	// Reentry confirmed. Build the entry.
	entry := k.Close
	buffer := e.cfg.StopBufferTicks * e.cfg.TickSize
	var stop, target, stopDist float64
	if e.setupSide == Short {
		stop = e.extreme + buffer
		stopDist = stop - entry
		target = entry - 2*stopDist
	} else {
		stop = e.extreme - buffer
		stopDist = entry - stop
		target = entry + 2*stopDist
	}

	// Filter: need a ready ATR to size it; skip if stop is too wide.
	if !atrReady || stopDist <= 0 || stopDist > e.cfg.MaxStopATR*atrVal {
		e.state = Watching
		return nil
	}

	// Trend filter. Default: skip a fade that fights a strong trend (Short in an
	// uptrend, Long in a downtrend). With TrendSkipWith, invert it: skip fades
	// that run WITH the trend and keep only counter-trend exhaustion fades. Only
	// applies once the EMAs are warm; during warmup the fade is allowed.
	if e.trendFilterOn() && e.emaReady && atrVal > 0 {
		strength := (e.fastVal - e.slowVal) / atrVal // >0 uptrend, <0 downtrend
		T := e.cfg.TrendThreshold
		var skip bool
		if e.cfg.TrendSkipWith {
			// skip Short in a downtrend, Long in an uptrend (with-trend fades)
			skip = (e.setupSide == Short && strength < -T) || (e.setupSide == Long && strength > T)
		} else {
			// skip Short in an uptrend, Long in a downtrend (counter-trend fades)
			skip = (e.setupSide == Short && strength > T) || (e.setupSide == Long && strength < -T)
		}
		if skip {
			e.state = Watching
			return nil
		}
	}

	e.pending = &pendingEntry{
		side: e.setupSide, time: k.CloseTime, entry: entry,
		stop: stop, target: target, stopDist: stopDist, atr: atrVal,
	}
	e.state = PendingEntry
	return &Proposal{
		Symbol: e.cfg.Symbol, Side: e.setupSide, Time: k.CloseTime,
		Entry: entry, Stop: stop, Target: target, StopDist: stopDist,
	}
}

// managePosition checks the candle against the stop and (in fixed-target mode)
// the target. The stop is always checked first using its value coming into the
// candle — so a trailing stop tightened from prior candles, never this candle's
// own favorable extreme (no lookahead). When both stop and target are touched in
// one candle, the stop is assumed hit first (conservative).
func (e *Engine) managePosition(k kline.Kline) *Trade {
	p := e.pos
	trailing := e.cfg.TrailATR > 0

	if p.side == Short {
		if k.High >= p.stop {
			return e.closePosition(p.stop, k.CloseTime, e.stopOutcome(trailing))
		}
		if trailing {
			if k.Low < p.bestLow {
				p.bestLow = k.Low
			}
			if cand := p.bestLow + e.cfg.TrailATR*p.atrAtEntry; cand < p.stop {
				p.stop = cand // ratchet down, locking in favorable movement
			}
		} else if k.Low <= p.target {
			return e.closePosition(p.target, k.CloseTime, OutcomeTarget)
		}
	} else {
		if k.Low <= p.stop {
			return e.closePosition(p.stop, k.CloseTime, e.stopOutcome(trailing))
		}
		if trailing {
			if k.High > p.bestHigh {
				p.bestHigh = k.High
			}
			if cand := p.bestHigh - e.cfg.TrailATR*p.atrAtEntry; cand > p.stop {
				p.stop = cand // ratchet up
			}
		} else if k.High >= p.target {
			return e.closePosition(p.target, k.CloseTime, OutcomeTarget)
		}
	}
	return nil
}

func (e *Engine) stopOutcome(trailing bool) Outcome {
	if trailing {
		return OutcomeTrail
	}
	return OutcomeStop
}

// closePosition finalizes the live position into a Trade and re-arms to
// WATCHING (same NY day). The realized R is computed from prices so EOD exits
// at an arbitrary price are scored correctly.
func (e *Engine) closePosition(exitPrice float64, exitTime time.Time, outcome Outcome) *Trade {
	p := e.pos
	var r float64
	if p.stopDist > 0 {
		if p.side == Short {
			r = (p.entry - exitPrice) / p.stopDist
		} else {
			r = (exitPrice - p.entry) / p.stopDist
		}
	}
	tr := &Trade{
		Symbol: e.cfg.Symbol, Side: p.side,
		EntryTime: p.entryTime, EntryPrice: p.entry,
		Stop: p.stop, Target: p.target, StopDist: p.stopDist,
		ExitTime: exitTime, ExitPrice: exitPrice, Outcome: outcome,
		R: r, ATRAtEntry: p.atrAtEntry,
	}
	e.pos = nil
	e.state = Watching // re-arm; per-side caps gate the next break
	return tr
}
