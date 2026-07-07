// Package structure implements a market-structure (Smart-Money-Concepts) engine:
// it detects swing pivots, tracks trend via higher-highs/higher-lows, and trades
// breaks of structure.
//
//   - BOS  (Break of Structure):  a close beyond the swing in the trend's
//     direction → trend continuation.
//   - CHoCH (Change of Character): a close beyond the last opposing swing,
//     against the trend → potential reversal (and the trend flips).
//
// Unlike the failed-break fade this is momentum/continuation: it enters on the
// break and rides with an ATR trailing stop, so winners can run. It is
// session-agnostic (no NY opening range, no midnight reset). It emits the same
// strategy.Proposal/Trade/StepResult types so it plugs into the existing
// portfolio backtest, costs, and tuner.
package structure

import (
	"math"
	"strings"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/indicator"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

// Config tunes the structure engine.
type Config struct {
	Symbol string
	PivotN int // bars on each side defining a swing (default 3)

	// SwingDevATR switches swing detection from fixed pivot-N to a ZigZag rule:
	// a high/low becomes a swing only once price reverses away from it by at
	// least this many ATRs. A "little dip" that pivot-N would mark as a HL/LH
	// never confirms, so only moves of real size define structure. 0 (default)
	// keeps pivot-N detection. Confirmation is causal: the swing is recognized
	// on the bar the reversal reaches the threshold, never retroactively.
	SwingDevATR     float64
	ATRPeriod       int     // Wilder ATR period (default 14)
	MaxStopATR      float64 // skip if stop distance > this × ATR (0 = no filter)
	MinStopFrac     float64 // skip if stop distance < this fraction of price (0 = no filter); avoids untradeably-tight stops that inflate fees-in-R
	TrailATR        float64 // ATR trailing stop (0 = use fixed TargetR target)
	TargetR         float64 // fixed R target when not trailing (default 2)
	StopBufferTicks float64 // buffer beyond the protective swing, in ticks
	TickSize        float64
	Signals         string // which breaks to trade: "both" (default), "bos", "choch"
	LongOnly        bool   // spot mode: only take long entries (skip shorts), structure still tracked

	// TrendModel selects how trend and CHoCH are defined:
	//
	//	"pivot" (default) — the original model: trend = direction of the last
	//	    break; any counter-trend close through the latest pivot swing is a
	//	    CHoCH. Simple, but labels internal structure: in ranges it flips
	//	    constantly, and the first bounce off a straight selloff reads CHoCH.
	//	"lux" — LuxAlgo Smart Money Concepts replication. Swings are one-sided
	//	    leg pivots: a bar whose high exceeds ALL LuxLen bars after it is a
	//	    swing high (no left-side condition — faster confirmation than
	//	    pivot-N), and a persistent leg state enforces strict H/L
	//	    alternation. Breaks use the naive flip semantics (close through the
	//	    last alternating swing; CHoCH when against the prior bias), exactly
	//	    like the indicator. SwingDevATR and PivotN are ignored for swings.
	//	"leg" — leg-based swing structure (proper SMC). Each trend leg has a
	//	    protected swing: in a downtrend the LOWER HIGH that led to the most
	//	    recent lower low (updated only when a NEW low is made — highs formed
	//	    while basing are internal and ignored); mirror for uptrends. CHoCH =
	//	    a close through the protected swing, which flips the trend; BOS = a
	//	    close through a pivot swing in the trend's direction. One CHoCH per
	//	    trend turn; ranges stay silent. StrictCHoCH is a pivot-model patch
	//	    and is ignored under "leg".
	TrendModel string

	// LuxLen is the leg length for TrendModel "lux": a swing confirms when a
	// bar's extreme exceeds all LuxLen bars after it. LuxAlgo's defaults are 50
	// (swing structure) and 5 (internal structure). Default 50.
	LuxLen int

	// StrictCHoCH requires a confirmed reversal sequence before a CHoCH counts:
	// a long CHoCH must break a genuine lower high (last two pivot highs
	// descending) AFTER a higher low has already formed (last two pivot lows
	// ascending) — i.e. HL in place, LH being broken; mirror for shorts. A break
	// against the trend without that sequence is NOT a change of character: it
	// is skipped, the trend does NOT flip, and the engine keeps waiting — a
	// later break of a fresh swing (with the sequence in place) can still
	// qualify. Prevents "CHoCH" labels on the first bounce inside a straight
	// selloff, where no structure has actually reversed.
	StrictCHoCH bool

	// Fair Value Gap entry. When UseFVG is set, instead of market-entering at the
	// break close, the engine rests a LIMIT order at the FVG (imbalance) left by
	// the breakout impulse and enters only if price retraces to fill it within
	// FVGMaxWaitBars bars. Better entry price + maker fee; fewer trades (some
	// breaks never retrace).
	UseFVG         bool
	FVGMaxWaitBars int // bars to wait for the limit to fill before cancelling (default 12)

	// FVGLookback is how many bars back an unmitigated gap may have formed and
	// still serve as the entry zone for a break. 1 (default, the original
	// behavior) accepts only a gap completing on the break candle itself; a
	// larger value lets the break use gaps left earlier in the impulse, as a
	// human would mark them. Gaps fully retraced through are dropped.
	FVGLookback int

	// BreakEven moves the stop to entry (break-even) once price reaches the
	// "prev high/low" — the swing level that was broken to create the signal.
	// Designed to pair with FVG retrace entry (entry sits below the prev high for
	// a long, so reaching it is a meaningful continuation milestone).
	BreakEven bool

	// FVGMidpoint enters at the midpoint of the gap instead of its near edge
	// (a deeper retrace: better price, lower fill rate).
	FVGMidpoint bool

	// FVGStop (requires UseFVG) places the stop just beyond the gap instead of
	// at the protective swing: for a long, FVGStopBufATR ATRs below the gap's
	// lower edge — if price trades through the whole imbalance, the setup is
	// invalid. Much tighter than a swing stop (bigger size per fixed risk, more
	// stop-outs, and fees weigh more per R — mind MinStopFrac).
	FVGStop       bool
	FVGStopBufATR float64 // buffer beyond the gap edge in ATRs (default 0.1)

	// MoveStopOnBOS trails the stop along structure: on each new break of
	// structure in the trade's direction, move the stop to the latest swing
	// (the new higher low for a long / lower high for a short).
	MoveStopOnBOS bool

	// PartialAtR is a one-shot partial take-profit: when the trade reaches this
	// many R of profit, bank PartialFraction of the position and move the stop
	// to break-even; the runner keeps the original target (or trail). The
	// trade's final R blends the banked part with the runner's exit. 0 = off.
	PartialAtR      float64
	PartialFraction float64 // fraction banked at the milestone (default 0.5)

	// RTrail is milestone R-trailing with the FULL position kept open (no
	// scale-outs, no target): when profit first reaches RTrailStart R the stop
	// moves to (RTrailStart − RTrailOffset) R; each further RTrailStep R
	// milestone ratchets the stop RTrailOffset behind it. Defaults 2/1/0.5:
	// reach 2R → stop 1.5R, reach 3R → stop 2.5R, and so on until the trail is
	// hit. Replaces the fixed target; ScaleOut takes precedence if both are set.
	RTrail       bool
	RTrailStart  float64
	RTrailStep   float64
	RTrailOffset float64

	// LiquiditySweep replaces structure-break entries with a liquidity-sweep
	// setup: price WICKS beyond a swing (grabbing the stops resting there) but
	// CLOSES back inside (rejection), then a high-impact FVG forms in the
	// reversal direction. Enter on that FVG; stop beyond the sweep wick. No
	// sweep, or no qualifying FVG within the window → no trade.
	LiquiditySweep   bool
	SweepMaxWaitBars int     // bars after a sweep to find a qualifying FVG (default 6)
	FVGMinATR        float64 // "high-impact": min FVG gap size in ATR units (0 = no size filter)

	// ScaleOut replaces the fixed TargetR/TrailATR exit with a laddered exit: at
	// every ScaleStepR milestone the engine banks ScaleFraction of the REMAINING
	// position and ratchets the stop one step behind (e.g. step 2R, fraction 0.5:
	// at +2R take 50% and stop→break-even; at +4R take 50% of the rest and
	// stop→+2R; at +6R stop→+4R; …). The final runner exits when that trailing
	// stop is hit. Trade.R is the size-weighted blend of all the partial exits.
	ScaleOut      bool
	ScaleStepR    float64 // R spacing between scale-outs (default 2)
	ScaleFraction float64 // fraction of the remaining position taken at each step (default 0.5)

	// ScaleTrailR places the stop this many R behind each reached milestone
	// (default ScaleStepR — one full step, the original behavior). 1 gives
	// "reach 2R → stop to 1R, reach 4R → stop to 3R, …".
	ScaleTrailR float64
	// ScaleMaxR caps the ladder: reaching this milestone closes the ENTIRE
	// remaining position there (outcome: target). 0 = ladder without end.
	ScaleMaxR float64

	// RequireSweep gates entries on a preceding liquidity grab: only take a break
	// that FOLLOWED a sweep of the opposite liquidity within SweepLookback bars —
	// a swept swing low (wick below, close back inside) before a long, a swept
	// swing high before a short. Filters breaks that were not set up by a stop-run.
	RequireSweep  bool
	SweepLookback int // bars a qualifying sweep may precede the break (default 10)

	// BlackoutSessions blocks entries around traditional-market session
	// boundaries, where opens/closes whipsaw price: a comma list of "asia"
	// (Tokyo 09:00–15:00 JST), "london" (08:00–16:30 UK), "us" (New York
	// 09:30–16:00 ET). No NEW entry is taken — and no resting limit is allowed
	// to fill — within SessionBufMin minutes of a listed session's open or
	// close (weekends exempt; zones are DST-aware). Empty = off.
	BlackoutSessions string
	SessionBufMin    int // blackout half-width in minutes (default 30)

	// HTFAlign is a LOOSE higher-timeframe filter: aggregate the primary candles
	// into HTFPeriod bars, track that timeframe's structure trend, and skip only
	// entries taken directly AGAINST it (a long while the HTF trend is bearish, a
	// short while bullish). A flat/unknown HTF trend blocks nothing.
	HTFAlign  bool
	HTFPeriod time.Duration // higher-timeframe bar length (e.g. 24h for daily)
	HTFPivotN int           // swing strength for the HTF trend (default 3)
}

func (c *Config) withDefaults() {
	if c.TrendModel == "" {
		c.TrendModel = "pivot"
	}
	if c.TrendModel == "lux" && c.LuxLen <= 0 {
		c.LuxLen = 50
	}
	if c.PivotN <= 0 {
		c.PivotN = 3
	}
	if c.ATRPeriod <= 0 {
		c.ATRPeriod = 14
	}
	if c.TargetR <= 0 {
		c.TargetR = 2
	}
	if c.Signals == "" {
		c.Signals = "both"
	}
	if (c.UseFVG || c.LiquiditySweep) && c.FVGMaxWaitBars <= 0 {
		c.FVGMaxWaitBars = 12
	}
	if (c.UseFVG || c.LiquiditySweep) && c.FVGLookback <= 0 {
		c.FVGLookback = 1
	}
	if c.LiquiditySweep && c.SweepMaxWaitBars <= 0 {
		c.SweepMaxWaitBars = 6
	}
	if c.ScaleOut {
		if c.ScaleStepR <= 0 {
			c.ScaleStepR = 2
		}
		if c.ScaleFraction <= 0 || c.ScaleFraction >= 1 {
			c.ScaleFraction = 0.5
		}
		if c.ScaleTrailR <= 0 {
			c.ScaleTrailR = c.ScaleStepR
		}
	}
	if c.RequireSweep && c.SweepLookback <= 0 {
		c.SweepLookback = 10
	}
	if c.PartialAtR > 0 && (c.PartialFraction <= 0 || c.PartialFraction >= 1) {
		c.PartialFraction = 0.5
	}
	if c.RTrail {
		if c.RTrailStart <= 0 {
			c.RTrailStart = 2
		}
		if c.RTrailStep <= 0 {
			c.RTrailStep = 1
		}
		if c.RTrailOffset <= 0 {
			c.RTrailOffset = 0.5
		}
	}
	if c.FVGStop && c.FVGStopBufATR <= 0 {
		c.FVGStopBufATR = 0.1
	}
	if c.BlackoutSessions != "" && c.SessionBufMin <= 0 {
		c.SessionBufMin = 30
	}
	if c.HTFAlign && c.HTFPivotN <= 0 {
		c.HTFPivotN = 3
	}
}

// tradeable reports whether a setup ("BOS"/"CHoCH") should be traded under the
// configured signal filter.
func (c *Config) tradeable(setup string) bool {
	switch c.Signals {
	case "bos":
		return setup == "BOS"
	case "choch":
		return setup == "CHoCH"
	default:
		return true
	}
}

type trendDir int

const (
	trendNone trendDir = iota
	trendBull
	trendBear
)

// StructureEvent is a classified break of structure (BOS or CHoCH): a candle
// whose close broke a swing level, tagged with the direction, the level broken,
// and the break price. It is emitted by Tracker (the screener's structure
// classifier) and carries no trading semantics.
type StructureEvent struct {
	Time  time.Time     // close time of the breaking candle
	Side  strategy.Side // Long = closed above a swing high, Short = closed below a swing low
	Setup string        // "BOS" (continuation) or "CHoCH" (reversal)
	Level float64       // the swing level that was broken
	Price float64       // the breaking candle's close
}

// Engine is the per-symbol market-structure state machine.
type Engine struct {
	cfg Config
	atr *indicator.ATR

	buf []kline.Kline // rolling window of the last 2*PivotN+1 candles

	haveSwingHigh, haveSwingLow     bool
	swingHigh, swingLow             float64
	swingHighTime, swingLowTime     time.Time // open time of each pivot candle (for events)
	swingHighBroken, swingLowBroken bool
	trend                           trendDir

	// Recent confirmed pivot levels (newest last), for the StrictCHoCH
	// sequence check: are highs descending / lows ascending (or vice versa)?
	pivHighs, pivLows []float64

	// Leg-model (TrendModel "leg") state. The protected swing is the level whose
	// break is a CHoCH: in a bear trend protHigh = the lower high that led to
	// the latest lower low; in a bull trend protLow = the higher low that led to
	// the latest higher high. maxSinceLow / minSinceHigh are the running extreme
	// since the latest pivot low/high formed (the leg's LH / HL candidate), and
	// extLow / extHigh are the running extreme of the whole trend (the strong
	// low/high a fresh CHoCH protects).
	protHigh, protLow           float64
	protHighT, protLowT         time.Time
	haveProtHigh, haveProtLow   bool
	maxSinceLow, minSinceHigh   float64
	maxSinceLowT, minSinceHighT time.Time
	extLow, extHigh             float64
	extLowT, extHighT           time.Time

	sweep   *sweepState
	limit   *restingLimit
	pending *pendingEntry
	pos     *position

	// RequireSweep tracking: bar index and the last bar a sweep of each side fired.
	bar               int
	lastLongSweepBar  int // last bar a swing low was swept (sell-side grab → long context)
	lastShortSweepBar int

	// HTFAlign tracking: the in-progress higher-timeframe candle and its trend.
	htfTracker                         *Tracker
	htfStarted                         bool
	htfBucket                          time.Time
	htfOpen, htfHigh, htfLow, htfClose float64

	// ZigZag swing detection (SwingDevATR > 0). zzExt is the current leg's
	// extreme (a high in an up leg, a low in a down leg) and zzCtr the running
	// counter-extreme since it — the candidate for the NEXT swing. zzDir: +1 up
	// leg, -1 down leg, 0 undetermined (before the first confirmation).
	zzInit         bool
	zzDir          int
	zzExt, zzCtr   float64
	zzExtT, zzCtrT time.Time
	lastATR        float64
	lastATRReady   bool

	// LuxAlgo-mode swing detection (TrendModel "lux"): rolling window of the
	// last LuxLen+1 candles and the persistent leg state (-1 unset, 0 bearish
	// leg, 1 bullish leg) that enforces swing alternation.
	luxBuf []kline.Kline
	luxLeg int

	// Recent unmitigated fair value gaps (oldest first), for FVGLookback.
	fvgs []fvgZone

	// Session blackout windows (from Config.BlackoutSessions), DST-aware.
	blackouts []sessionWindow

	// Feature-snapshot support: rolling volume window and the bar of the last
	// trend flip (trade features for offline confidence training).
	vols        []float64
	lastFlipBar int

	// Observability (see events.go). lastK is the candle currently being
	// stepped, so events emitted outside Step (e.g. Resolve) can be timestamped.
	onEvent func(Event)
	lastK   kline.Kline
}

// sessionWindow is one traditional-market session whose open/close boundaries
// are blacked out for entries.
type sessionWindow struct {
	loc            *time.Location
	openH, openM   int
	closeH, closeM int
}

// sessionDefs are the supported BlackoutSessions names.
var sessionDefs = map[string]struct {
	tz             string
	openH, openM   int
	closeH, closeM int
}{
	"asia":   {"Asia/Tokyo", 9, 0, 15, 0},
	"london": {"Europe/London", 8, 0, 16, 30},
	"us":     {"America/New_York", 9, 30, 16, 0},
}

// inBlackout reports whether t falls within SessionBufMin minutes of a listed
// session's open or close (weekends exempt in that session's own time zone).
func (e *Engine) inBlackout(t time.Time) bool {
	if len(e.blackouts) == 0 {
		return false
	}
	buf := time.Duration(e.cfg.SessionBufMin) * time.Minute
	for _, s := range e.blackouts {
		lt := t.In(s.loc)
		if wd := lt.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		open := time.Date(lt.Year(), lt.Month(), lt.Day(), s.openH, s.openM, 0, 0, s.loc)
		cls := time.Date(lt.Year(), lt.Month(), lt.Day(), s.closeH, s.closeM, 0, 0, s.loc)
		if absDur(t.Sub(open)) <= buf || absDur(t.Sub(cls)) <= buf {
			return true
		}
	}
	return false
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// fvgZone is an unmitigated fair value gap: [lo,hi] in the impulse's wake,
// alive until price retraces through the whole gap.
type fvgZone struct {
	side    strategy.Side
	lo, hi  float64
	born    int       // bar index when the gap completed (its 3rd candle)
	formedT time.Time // open time of the gap's first candle (for drawing)
}

// sweepState tracks a liquidity sweep awaiting a high-impact FVG to enter on.
type sweepState struct {
	side     strategy.Side // Long = swept lows, Short = swept highs
	level    float64       // the swing level that was swept (for invalidation)
	stopRef  float64       // the sweep wick extreme (stop goes beyond this)
	barsLeft int
}

// restingLimit is an FVG limit order waiting to be filled on a retrace.
type restingLimit struct {
	feats    map[string]float64
	side     strategy.Side
	setup    string
	level    float64 // limit price (gap edge)
	stop     float64
	target   float64
	stopDist float64
	trigger  float64 // prev swing level → break-even trigger
	atr      float64
	barsLeft int
}

type pendingEntry struct {
	feats    map[string]float64
	side     strategy.Side
	setup    string
	time     time.Time
	entry    float64
	stop     float64
	target   float64
	stopDist float64
	trigger  float64
	atr      float64
	maker    bool
}

type position struct {
	side        strategy.Side
	setup       string
	entryTime   time.Time
	entry       float64
	stop        float64
	target      float64
	stopDist    float64
	feats       map[string]float64
	trigger     float64
	beArmed     bool
	partialDone bool
	atrAtEntry  float64
	bestLow     float64
	bestHigh    float64
	maker       bool

	// laddered scale-out state (ScaleOut mode)
	remaining float64 // fraction of the position still open (starts 1.0)
	scaleK    int     // scale-outs banked so far (milestone index)
	realizedR float64 // R already banked from partial exits (size-weighted)
	rtK       int     // R-trail milestones reached (RTrail mode)
}

// New builds a structure engine.
func New(cfg Config) *Engine {
	cfg.withDefaults()
	inf := math.Inf(1)
	e := &Engine{
		cfg: cfg, atr: indicator.NewATR(cfg.ATRPeriod),
		lastLongSweepBar: -1 << 30, lastShortSweepBar: -1 << 30, // no sweep seen yet
		maxSinceLow: -inf, minSinceHigh: inf, extLow: inf, extHigh: -inf,
		luxLeg: -1,
	}
	if cfg.HTFAlign {
		e.htfTracker = NewTracker(cfg.HTFPivotN)
	}
	for _, name := range strings.Split(cfg.BlackoutSessions, ",") {
		def, ok := sessionDefs[strings.TrimSpace(strings.ToLower(name))]
		if !ok {
			continue
		}
		loc, err := time.LoadLocation(def.tz)
		if err != nil {
			continue
		}
		e.blackouts = append(e.blackouts, sessionWindow{
			loc: loc, openH: def.openH, openM: def.openM, closeH: def.closeH, closeM: def.closeM,
		})
	}
	return e
}

// Symbol returns the engine's symbol.
func (e *Engine) Symbol() string { return e.cfg.Symbol }

// OpenPosition returns a snapshot of the live position, or nil if flat.
func (e *Engine) OpenPosition() *strategy.OpenPosition {
	if e.pos == nil {
		return nil
	}
	return &strategy.OpenPosition{
		Symbol: e.cfg.Symbol, Side: e.pos.side,
		Entry: e.pos.entry, Stop: e.pos.stop, StopDist: e.pos.stopDist,
	}
}

// Step advances by one candle: update ATR and swings, manage any open position,
// else watch for a structure break.
func (e *Engine) Step(k kline.Kline) strategy.StepResult {
	e.lastK = k
	e.vols = append(e.vols, k.Volume)
	if len(e.vols) > 20 {
		e.vols = e.vols[1:]
	}
	atrVal, atrReady := e.atr.Update(k)
	e.lastATR, e.lastATRReady = atrVal, atrReady
	e.updateSwings(k)
	e.updateLegTrackers(k)
	e.bar++
	e.trackFVGs(k)
	if e.cfg.HTFAlign {
		e.updateHTF(k)
	}
	if e.cfg.RequireSweep {
		e.recordSweep(k)
	}

	if e.pos != nil {
		if tr := e.managePosition(k); tr != nil {
			return strategy.StepResult{Closed: tr}
		}
		return strategy.StepResult{}
	}

	// A resting FVG limit order takes priority: try to fill it, else age it out.
	if e.limit != nil {
		if p := e.handleLimit(k); p != nil {
			return strategy.StepResult{Proposal: p}
		}
		return strategy.StepResult{}
	}

	// Liquidity-sweep mode replaces structure-break entries entirely.
	if e.cfg.LiquiditySweep {
		if e.sweep != nil {
			e.handleSweep(k, atrVal, atrReady)
		} else {
			e.detectSweep(k)
		}
		return strategy.StepResult{}
	}

	if e.cfg.TrendModel == "leg" {
		return e.legDetect(k, atrVal, atrReady)
	}

	// A close beyond a fresh swing high (long) or low (short) is a structure
	// break. Just-confirmed swings cannot trigger on the same bar (the pivot
	// condition guarantees the current high/low is inside the pivot).
	if e.haveSwingHigh && !e.swingHighBroken && k.Close > e.swingHigh {
		return e.tryEnter(k, strategy.Long, atrVal, atrReady)
	}
	if e.haveSwingLow && !e.swingLowBroken && k.Close < e.swingLow {
		return e.tryEnter(k, strategy.Short, atrVal, atrReady)
	}
	return strategy.StepResult{}
}

// updateLegTrackers maintains the leg-model running extremes. maxSinceLow /
// minSinceHigh restart from each new pivot (see updateSwings) and track the
// extreme of the current leg; extLow / extHigh track the extreme of the whole
// trend and freeze while the trend points away from them (a bear trend's
// extHigh is fixed at the top it broke down from until a CHoCH resets it).
//
// The protected swing promotes here, on trend-extreme extension: when a bear
// trend prints a NEW low and a pivot high had formed since the previous low,
// that pivot high is the lower high the new leg came from — the CHoCH level.
// A straight decline (no rally in between) promotes nothing, and highs formed
// while basing (no new low after them) never become the CHoCH level. Mirror
// for bull trends.
func (e *Engine) updateLegTrackers(k kline.Kline) {
	if k.High > e.maxSinceLow {
		e.maxSinceLow, e.maxSinceLowT = k.High, k.OpenTime
	}
	if k.Low < e.minSinceHigh {
		e.minSinceHigh, e.minSinceHighT = k.Low, k.OpenTime
	}
	if e.trend != trendBull && k.Low < e.extLow {
		if e.trend == trendBear && e.haveSwingHigh && e.swingHighTime.After(e.extLowT) {
			e.protHigh, e.protHighT, e.haveProtHigh = e.swingHigh, e.swingHighTime, true
		}
		e.extLow, e.extLowT = k.Low, k.OpenTime
	}
	if e.trend != trendBear && k.High > e.extHigh {
		if e.trend == trendBull && e.haveSwingLow && e.swingLowTime.After(e.extHighT) {
			e.protLow, e.protLowT, e.haveProtLow = e.swingLow, e.swingLowTime, true
		}
		e.extHigh, e.extHighT = k.High, k.OpenTime
	}
}

// legDetect is the TrendModel "leg" break detector. CHoCH = a close through the
// protected swing (one per trend turn; flips the trend). BOS = a close through
// a pivot swing in the trend's direction (extends the trend and re-anchors the
// protected swing to the leg's origin). Counter-trend pivot breaks that do NOT
// reach the protected swing are internal structure: consumed silently, no
// label, no trend change — so ranges stay quiet.
func (e *Engine) legDetect(k kline.Kline, atrVal float64, atrReady bool) strategy.StepResult {
	switch e.trend {
	case trendBear:
		// CHoCH long: close above the lower high that led to the trend's low.
		if e.haveProtHigh && k.Close > e.protHigh {
			lvl, lvlT := e.protHigh, e.protHighT
			e.setTrend(trendBull)
			e.haveProtHigh = false
			// The new uptrend's protected low is the bottom it reversed from.
			e.protLow, e.protLowT, e.haveProtLow = e.extLow, e.extLowT, true
			e.extHigh, e.extHighT = k.High, k.OpenTime
			e.emit(Event{Type: "break", Time: k.OpenTime, Time2: lvlT, Side: "long", Setup: "CHoCH", Level: lvl, Price: k.Close})
			return e.proposeEntry(k, strategy.Long, "CHoCH", lvl, atrVal, atrReady)
		}
		// BOS short: close below the latest pivot low extends the downtrend.
		// (The protected high promotes in updateLegTrackers, when the trend
		// actually prints its new low.)
		if e.haveSwingLow && !e.swingLowBroken && k.Close < e.swingLow {
			e.swingLowBroken = true
			lvl, lvlT := e.swingLow, e.swingLowTime
			e.emit(Event{Type: "break", Time: k.OpenTime, Time2: lvlT, Side: "short", Setup: "BOS", Level: lvl, Price: k.Close})
			return e.proposeEntry(k, strategy.Short, "BOS", lvl, atrVal, atrReady)
		}
		// Internal: an upside pivot break that never reached the protected high.
		if e.haveSwingHigh && !e.swingHighBroken && k.Close > e.swingHigh {
			e.swingHighBroken = true
		}

	case trendBull:
		if e.haveProtLow && k.Close < e.protLow {
			lvl, lvlT := e.protLow, e.protLowT
			e.setTrend(trendBear)
			e.haveProtLow = false
			e.protHigh, e.protHighT, e.haveProtHigh = e.extHigh, e.extHighT, true
			e.extLow, e.extLowT = k.Low, k.OpenTime
			e.emit(Event{Type: "break", Time: k.OpenTime, Time2: lvlT, Side: "short", Setup: "CHoCH", Level: lvl, Price: k.Close})
			return e.proposeEntry(k, strategy.Short, "CHoCH", lvl, atrVal, atrReady)
		}
		if e.haveSwingHigh && !e.swingHighBroken && k.Close > e.swingHigh {
			e.swingHighBroken = true
			lvl, lvlT := e.swingHigh, e.swingHighTime
			e.emit(Event{Type: "break", Time: k.OpenTime, Time2: lvlT, Side: "long", Setup: "BOS", Level: lvl, Price: k.Close})
			return e.proposeEntry(k, strategy.Long, "BOS", lvl, atrVal, atrReady)
		}
		if e.haveSwingLow && !e.swingLowBroken && k.Close < e.swingLow {
			e.swingLowBroken = true
		}

	default:
		// No trend yet: seed from the first pivot break, protecting the leg's
		// origin on the other side.
		if e.haveSwingHigh && !e.swingHighBroken && k.Close > e.swingHigh {
			e.swingHighBroken = true
			lvl, lvlT := e.swingHigh, e.swingHighTime
			e.setTrend(trendBull)
			e.protLow, e.protLowT, e.haveProtLow = e.minSinceHigh, e.minSinceHighT, true
			e.extHigh, e.extHighT = k.High, k.OpenTime
			e.emit(Event{Type: "break", Time: k.OpenTime, Time2: lvlT, Side: "long", Setup: "BOS", Level: lvl, Price: k.Close})
			return e.proposeEntry(k, strategy.Long, "BOS", lvl, atrVal, atrReady)
		}
		if e.haveSwingLow && !e.swingLowBroken && k.Close < e.swingLow {
			e.swingLowBroken = true
			lvl, lvlT := e.swingLow, e.swingLowTime
			e.setTrend(trendBear)
			e.protHigh, e.protHighT, e.haveProtHigh = e.maxSinceLow, e.maxSinceLowT, true
			e.extLow, e.extLowT = k.Low, k.OpenTime
			e.emit(Event{Type: "break", Time: k.OpenTime, Time2: lvlT, Side: "short", Setup: "BOS", Level: lvl, Price: k.Close})
			return e.proposeEntry(k, strategy.Short, "BOS", lvl, atrVal, atrReady)
		}
	}
	return strategy.StepResult{}
}

// updateHTF folds the primary candle into the current higher-timeframe bar
// (bucketed by HTFPeriod). When a bar completes it feeds the CLOSED HTF candle to
// the HTF trend tracker — never the in-progress one, so the trend uses no
// look-ahead. Buckets align to the epoch, so 24h = midnight-UTC daily bars.
func (e *Engine) updateHTF(k kline.Kline) {
	bucket := k.OpenTime.Truncate(e.cfg.HTFPeriod)
	switch {
	case !e.htfStarted:
		e.htfStarted = true
	case bucket != e.htfBucket:
		e.htfTracker.Update(kline.Kline{
			OpenTime: e.htfBucket, Open: e.htfOpen, High: e.htfHigh, Low: e.htfLow,
			Close: e.htfClose, CloseTime: bucket,
		})
	default:
		if k.High > e.htfHigh {
			e.htfHigh = k.High
		}
		if k.Low < e.htfLow {
			e.htfLow = k.Low
		}
		e.htfClose = k.Close
		return
	}
	// start a fresh HTF bar (first candle, or a new bucket)
	e.htfBucket = bucket
	e.htfOpen, e.htfHigh, e.htfLow, e.htfClose = k.Open, k.High, k.Low, k.Close
}

// recordSweep timestamps the latest liquidity grab of each side: a wick beyond a
// swing that closes back inside (a stop-run + rejection), without consuming the
// swing — it only marks that a sweep happened, for the RequireSweep entry gate.
func (e *Engine) recordSweep(k kline.Kline) {
	if e.haveSwingLow && k.Low < e.swingLow && k.Close > e.swingLow {
		e.lastLongSweepBar = e.bar // sell-side liquidity taken → long context
	}
	if e.haveSwingHigh && k.High > e.swingHigh && k.Close < e.swingHigh {
		e.lastShortSweepBar = e.bar
	}
}

// detectSweep arms a sweep when a candle wicks beyond a swing but closes back
// inside it (a stop-hunt / liquidity grab + rejection).
func (e *Engine) detectSweep(k kline.Kline) {
	switch {
	case e.haveSwingLow && !e.swingLowBroken && k.Low < e.swingLow && k.Close > e.swingLow:
		e.swingLowBroken = true // consume this level
		e.sweep = &sweepState{side: strategy.Long, level: e.swingLow, stopRef: k.Low, barsLeft: e.cfg.SweepMaxWaitBars}
		e.emit(Event{Type: "sweep", Time: k.OpenTime, Side: "long", Level: e.swingLow, Price: k.Low})
	case e.haveSwingHigh && !e.swingHighBroken && k.High > e.swingHigh && k.Close < e.swingHigh:
		e.swingHighBroken = true
		e.sweep = &sweepState{side: strategy.Short, level: e.swingHigh, stopRef: k.High, barsLeft: e.cfg.SweepMaxWaitBars}
		e.emit(Event{Type: "sweep", Time: k.OpenTime, Side: "short", Level: e.swingHigh, Price: k.High})
	}
}

// handleSweep, after a sweep, waits for a high-impact FVG in the reversal
// direction and rests a limit on it (stop beyond the sweep wick). Cancels on a
// decisive close back through the swept level (a real break, not a sweep) or on
// timeout.
func (e *Engine) handleSweep(k kline.Kline, atrVal float64, atrReady bool) {
	s := e.sweep
	// Invalidation: a close beyond the swept level means it broke, not swept.
	if (s.side == strategy.Long && k.Close < s.level) || (s.side == strategy.Short && k.Close > s.level) {
		e.sweep = nil
		return
	}
	lvl, z, ok := e.detectFVG(s.side)
	fvgLo, fvgHi := z.lo, z.hi
	highImpact := ok && atrReady && (e.cfg.FVGMinATR <= 0 || fvgHi-fvgLo >= e.cfg.FVGMinATR*atrVal)
	if highImpact {
		buffer := e.cfg.StopBufferTicks * e.cfg.TickSize
		var stop, stopDist float64
		if s.side == strategy.Long {
			stop = s.stopRef - buffer
			stopDist = lvl - stop
		} else {
			stop = s.stopRef + buffer
			stopDist = stop - lvl
		}
		tooTight := e.cfg.MinStopFrac > 0 && stopDist < e.cfg.MinStopFrac*lvl
		tooWide := e.cfg.MaxStopATR > 0 && stopDist > e.cfg.MaxStopATR*atrVal
		if stopDist > 0 && !tooTight && !tooWide {
			var target float64
			if s.side == strategy.Long {
				target = lvl + e.cfg.TargetR*stopDist
			} else {
				target = lvl - e.cfg.TargetR*stopDist
			}
			e.limit = &restingLimit{
				side: s.side, setup: "SWEEP", level: lvl, stop: stop, target: target,
				stopDist: stopDist, trigger: s.level, atr: atrVal, barsLeft: e.cfg.FVGMaxWaitBars,
			}
			e.emit(Event{
				Type: "fvg_limit", Time: k.OpenTime, Time2: z.formedT,
				Side: sideStr(s.side), Setup: "SWEEP", Level: lvl, Lo: fvgLo, Hi: fvgHi,
				Stop: stop, Target: target,
			})
			e.sweep = nil
			return
		}
	}
	if s.barsLeft--; s.barsLeft <= 0 {
		e.sweep = nil
	}
}

// handleLimit checks whether the resting FVG limit fills this bar (price touched
// the level) and, if so, proposes a maker entry. Otherwise it ages the order out
// and cancels it on expiry or if the bar closes beyond the protective stop.
func (e *Engine) handleLimit(k kline.Kline) *strategy.Proposal {
	l := e.limit
	filled := (l.side == strategy.Long && k.Low <= l.level) ||
		(l.side == strategy.Short && k.High >= l.level)
	// A fill landing in a session blackout is exactly the whipsaw the filter
	// exists to avoid: cancel the order instead of taking it.
	if filled && e.inBlackout(k.CloseTime) {
		e.limit = nil
		e.emit(Event{Type: "limit_cancelled", Time: k.OpenTime, Side: sideStr(l.side), Setup: l.setup, Price: l.level, Reason: "session_blackout"})
		return nil
	}
	if filled {
		e.pending = &pendingEntry{
			feats: l.feats,
			side:  l.side, setup: l.setup, time: k.CloseTime, entry: l.level,
			stop: l.stop, target: l.target, stopDist: l.stopDist, trigger: l.trigger,
			atr: l.atr, maker: true,
		}
		e.limit = nil
		e.emit(Event{Type: "limit_filled", Time: k.OpenTime, Side: sideStr(l.side), Setup: l.setup, Price: l.level})
		return &strategy.Proposal{
			Symbol: e.cfg.Symbol, Side: l.side, Time: k.CloseTime,
			Entry: l.level, Stop: l.stop, Target: l.target, StopDist: l.stopDist,
		}
	}
	l.barsLeft--
	invalid := (l.side == strategy.Long && k.Close < l.stop) ||
		(l.side == strategy.Short && k.Close > l.stop)
	if l.barsLeft <= 0 || invalid {
		e.limit = nil // opportunity gone
		reason := "expired"
		if invalid {
			reason = "invalidated"
		}
		e.emit(Event{Type: "limit_cancelled", Time: k.OpenTime, Side: sideStr(l.side), Setup: l.setup, Price: l.level, Reason: reason})
	}
	return nil
}

// updateSwings appends the candle (the buffer also feeds FVG detection) and
// confirms swings: ZigZag when SwingDevATR is set, else a pivot N bars back.
func (e *Engine) updateSwings(k kline.Kline) {
	win := 2*e.cfg.PivotN + 1
	e.buf = append(e.buf, k)
	if len(e.buf) > win {
		e.buf = e.buf[1:]
	}
	if e.cfg.TrendModel == "lux" {
		e.updateSwingsLux(k)
		return
	}
	if e.cfg.SwingDevATR > 0 {
		e.updateSwingsZigZag(k)
		return
	}
	if len(e.buf) < win {
		return
	}
	c := e.cfg.PivotN
	center := e.buf[c]
	isHigh, isLow := true, true
	for i := range e.buf {
		if i == c {
			continue
		}
		if e.buf[i].High >= center.High {
			isHigh = false
		}
		if e.buf[i].Low <= center.Low {
			isLow = false
		}
	}
	if isHigh {
		e.swingHigh, e.haveSwingHigh, e.swingHighBroken = center.High, true, false
		e.swingHighTime = center.OpenTime
		e.pivHighs = appendCap(e.pivHighs, center.High)
		// Restart the leg's HL candidate: lowest low since this pivot high.
		e.minSinceHigh, e.minSinceHighT = e.buf[c+1].Low, e.buf[c+1].OpenTime
		for _, b := range e.buf[c+2:] {
			if b.Low < e.minSinceHigh {
				e.minSinceHigh, e.minSinceHighT = b.Low, b.OpenTime
			}
		}
		e.emit(Event{Type: "pivot_high", Time: center.OpenTime, Price: center.High})
	}
	if isLow {
		e.swingLow, e.haveSwingLow, e.swingLowBroken = center.Low, true, false
		e.swingLowTime = center.OpenTime
		e.pivLows = appendCap(e.pivLows, center.Low)
		// Restart the leg's LH candidate: highest high since this pivot low.
		e.maxSinceLow, e.maxSinceLowT = e.buf[c+1].High, e.buf[c+1].OpenTime
		for _, b := range e.buf[c+2:] {
			if b.High > e.maxSinceLow {
				e.maxSinceLow, e.maxSinceLowT = b.High, b.OpenTime
			}
		}
		e.emit(Event{Type: "pivot_low", Time: center.OpenTime, Price: center.Low})
	}
}

// updateSwingsZigZag confirms a swing only after price reverses from the leg's
// extreme by SwingDevATR × ATR. In an up leg zzExt tracks the highest high and
// zzCtr the lowest low since it; when ext−ctr reaches the threshold, the
// extreme is confirmed as the swing high, the counter-extreme becomes the new
// (down) leg's extreme, and the roles mirror.
func (e *Engine) updateSwingsZigZag(k kline.Kline) {
	if !e.zzInit {
		e.zzInit = true
		e.zzExt, e.zzExtT = k.High, k.OpenTime // running high candidate
		e.zzCtr, e.zzCtrT = k.Low, k.OpenTime  // running low candidate
		return
	}
	dev := e.cfg.SwingDevATR * e.lastATR
	ready := e.lastATRReady && dev > 0

	switch e.zzDir {
	case 0: // no leg yet: whichever candidate deviates first sets the direction
		if k.High > e.zzExt {
			e.zzExt, e.zzExtT = k.High, k.OpenTime
		}
		if k.Low < e.zzCtr {
			e.zzCtr, e.zzCtrT = k.Low, k.OpenTime
		}
		if !ready {
			return
		}
		switch {
		case e.zzExt-k.Low >= dev:
			// Fell away from the high → it was a swing high. Adopt the up-leg
			// convention (ctr = low since the high) before confirming.
			if !e.zzCtrT.After(e.zzExtT) {
				e.zzCtr, e.zzCtrT = k.Low, k.OpenTime
			}
			e.zzConfirmHigh(k)
		case k.High-e.zzCtr >= dev:
			// Rallied away from the low → swing low. Adopt the down-leg
			// convention (ext = the low, ctr = high since it).
			lo, loT := e.zzCtr, e.zzCtrT
			hi, hiT := e.zzExt, e.zzExtT
			if !hiT.After(loT) {
				hi, hiT = k.High, k.OpenTime
			}
			e.zzExt, e.zzExtT = lo, loT
			e.zzCtr, e.zzCtrT = hi, hiT
			e.zzConfirmLow(k)
		}
	case 1: // up leg: ext = highest high, ctr = lowest low since it
		if k.High > e.zzExt {
			e.zzExt, e.zzExtT = k.High, k.OpenTime
			e.zzCtr, e.zzCtrT = k.Low, k.OpenTime
		} else if k.Low < e.zzCtr {
			e.zzCtr, e.zzCtrT = k.Low, k.OpenTime
		}
		if ready && e.zzExt-e.zzCtr >= dev {
			e.zzConfirmHigh(k)
		}
	case -1: // down leg: ext = lowest low, ctr = highest high since it
		if k.Low < e.zzExt {
			e.zzExt, e.zzExtT = k.Low, k.OpenTime
			e.zzCtr, e.zzCtrT = k.High, k.OpenTime
		} else if k.High > e.zzCtr {
			e.zzCtr, e.zzCtrT = k.High, k.OpenTime
		}
		if ready && e.zzCtr-e.zzExt >= dev {
			e.zzConfirmLow(k)
		}
	}
}

// zzConfirmHigh commits the up leg's extreme as the swing high and starts a
// down leg from the counter-extreme (the lowest low seen since that high).
func (e *Engine) zzConfirmHigh(k kline.Kline) {
	e.swingHigh, e.swingHighTime, e.haveSwingHigh, e.swingHighBroken = e.zzExt, e.zzExtT, true, false
	e.pivHighs = appendCap(e.pivHighs, e.zzExt)
	e.minSinceHigh, e.minSinceHighT = e.zzCtr, e.zzCtrT
	e.emit(Event{Type: "pivot_high", Time: e.zzExtT, Price: e.zzExt})
	e.zzDir = -1
	e.zzExt, e.zzExtT = e.zzCtr, e.zzCtrT // the dip low starts the down leg
	e.zzCtr, e.zzCtrT = k.High, k.OpenTime
}

// zzConfirmLow commits the down leg's extreme as the swing low and starts an
// up leg from the counter-extreme (the highest high seen since that low).
func (e *Engine) zzConfirmLow(k kline.Kline) {
	e.swingLow, e.swingLowTime, e.haveSwingLow, e.swingLowBroken = e.zzExt, e.zzExtT, true, false
	e.pivLows = appendCap(e.pivLows, e.zzExt)
	e.maxSinceLow, e.maxSinceLowT = e.zzCtr, e.zzCtrT
	e.emit(Event{Type: "pivot_low", Time: e.zzExtT, Price: e.zzExt})
	e.zzDir = 1
	e.zzExt, e.zzExtT = e.zzCtr, e.zzCtrT // the rally high starts the up leg
	e.zzCtr, e.zzCtrT = k.Low, k.OpenTime
}

// updateSwingsLux replicates LuxAlgo SMC swing detection: the bar LuxLen bars
// back is a swing high when its high exceeds the highs of ALL LuxLen bars
// after it (one-sided — no left-side condition), and a swing low mirror-wise;
// a persistent leg state registers only the FIRST flip in each direction, so
// confirmed swings strictly alternate high, low, high, low.
func (e *Engine) updateSwingsLux(k kline.Kline) {
	n := e.cfg.LuxLen + 1
	e.luxBuf = append(e.luxBuf, k)
	if len(e.luxBuf) > n {
		e.luxBuf = e.luxBuf[1:]
	}
	if len(e.luxBuf) < n {
		return
	}
	ref := e.luxBuf[0] // the bar LuxLen bars ago
	hi, hiT := e.luxBuf[1].High, e.luxBuf[1].OpenTime
	lo, loT := e.luxBuf[1].Low, e.luxBuf[1].OpenTime
	for _, b := range e.luxBuf[2:] {
		if b.High > hi {
			hi, hiT = b.High, b.OpenTime
		}
		if b.Low < lo {
			lo, loT = b.Low, b.OpenTime
		}
	}
	// Pine: if newLegHigh → bearish leg, else if newLegLow → bullish leg;
	// a swing registers only when the leg CHANGES.
	switch {
	case ref.High > hi && e.luxLeg != 0:
		e.luxLeg = 0
		e.swingHigh, e.swingHighTime, e.haveSwingHigh, e.swingHighBroken = ref.High, ref.OpenTime, true, false
		e.pivHighs = appendCap(e.pivHighs, ref.High)
		e.minSinceHigh, e.minSinceHighT = lo, loT
		e.emit(Event{Type: "pivot_high", Time: ref.OpenTime, Price: ref.High})
	case ref.High <= hi && ref.Low < lo && e.luxLeg != 1:
		e.luxLeg = 1
		e.swingLow, e.swingLowTime, e.haveSwingLow, e.swingLowBroken = ref.Low, ref.OpenTime, true, false
		e.pivLows = appendCap(e.pivLows, ref.Low)
		e.maxSinceLow, e.maxSinceLowT = hi, hiT
		e.emit(Event{Type: "pivot_low", Time: ref.OpenTime, Price: ref.Low})
	}
}

// setTrend records trend flips (feature: trend age at entry).
func (e *Engine) setTrend(t trendDir) {
	if e.trend != t {
		e.lastFlipBar = e.bar
	}
	e.trend = t
}

// features snapshots the decision context at signal time, normalized so values
// compare across symbols (ATR multiples, ratios, z-scores). Used for offline
// confidence-model training; must only use information available at the close
// of the signal candle.
func (e *Engine) features(k kline.Kline, side strategy.Side, setup string,
	entry, stopDist, fvgLo, fvgHi, atrVal float64) map[string]float64 {
	f := map[string]float64{}
	b := func(name string, cond bool) {
		if cond {
			f[name] = 1
		} else {
			f[name] = 0
		}
	}
	b("setup_choch", setup == "CHoCH")
	b("side_long", side == strategy.Long)
	if atrVal > 0 {
		f["stop_atr"] = stopDist / atrVal
		f["gap_atr"] = (fvgHi - fvgLo) / atrVal
		f["entry_dist_atr"] = math.Abs(k.Close-entry) / atrVal
		f["body_atr"] = math.Abs(k.Close-k.Open) / atrVal
		f["range_atr"] = (k.High - k.Low) / atrVal
	}
	if entry > 0 {
		f["stop_pct"] = stopDist / entry * 100
	}
	if k.Close > 0 {
		f["atr_pct"] = atrVal / k.Close * 100
	}
	if rng := k.High - k.Low; rng > 0 {
		f["close_pos"] = (k.Close - k.Low) / rng
	}
	// volume z-score vs the rolling window
	if n := len(e.vols); n >= 10 {
		var mean, sq float64
		for _, v := range e.vols {
			mean += v
		}
		mean /= float64(n)
		for _, v := range e.vols {
			sq += (v - mean) * (v - mean)
		}
		if sd := math.Sqrt(sq / float64(n)); sd > 0 {
			f["vol_z"] = (k.Volume - mean) / sd
		}
	}
	f["hour"] = float64(k.OpenTime.UTC().Hour())
	f["dow"] = float64(k.OpenTime.UTC().Weekday())
	f["trend_age"] = float64(e.bar - e.lastFlipBar)
	if e.htfTracker != nil {
		dir := 1.0
		if side == strategy.Short {
			dir = -1
		}
		f["htf_align"] = dir * float64(e.htfTracker.Trend())
	}
	return f
}

func appendCap(s []float64, v float64) []float64 {
	s = append(s, v)
	if len(s) > 4 {
		s = s[1:]
	}
	return s
}

// chochConfirmed reports whether the pivot sequence supports a genuine change
// of character. Both directions reduce to the same shape — structure must have
// compressed against the old trend before the break:
//
//	long CHoCH (bear→bull): a higher low formed (lows ascending) and the level
//	    being broken is a true lower high (highs descending);
//	short CHoCH (bull→bear): a lower high formed (highs descending) and the
//	    level being broken is a true higher low (lows ascending).
func (e *Engine) chochConfirmed() bool {
	nh, nl := len(e.pivHighs), len(e.pivLows)
	if nh < 2 || nl < 2 {
		return false
	}
	return e.pivHighs[nh-1] < e.pivHighs[nh-2] && e.pivLows[nl-1] > e.pivLows[nl-2]
}

// tryEnter handles a structure break: it always consumes the level and updates
// the trend (a structural event), then proposes an entry if a protective swing
// exists, the stop is sane, and the ATR filter passes.
func (e *Engine) tryEnter(k kline.Kline, side strategy.Side, atrVal float64, atrReady bool) strategy.StepResult {
	// Classify BOS vs CHoCH from the trend before flipping it.
	setup := "BOS"
	broken, brokenT := e.swingHigh, e.swingHighTime
	if side == strategy.Long {
		if e.trend == trendBear {
			setup = "CHoCH"
		}
	} else {
		if e.trend == trendBull {
			setup = "CHoCH"
		}
		broken, brokenT = e.swingLow, e.swingLowTime
	}

	// StrictCHoCH: without the reversal sequence (HL formed, LH being broken —
	// or the mirror) the break is not a change of character. Consume the level
	// but keep the trend: a later, structurally confirmed break can still fire.
	unconfirmed := setup == "CHoCH" && e.cfg.StrictCHoCH && !e.chochConfirmed()

	if side == strategy.Long {
		e.swingHighBroken = true
		if !unconfirmed {
			e.setTrend(trendBull)
		}
	} else {
		e.swingLowBroken = true
		if !unconfirmed {
			e.setTrend(trendBear)
		}
	}
	// Time2 = the broken pivot's candle, so a UI can draw the level from where
	// the swing formed to where it broke.
	e.emit(Event{Type: "break", Time: k.OpenTime, Time2: brokenT, Side: sideStr(side), Setup: setup, Level: broken, Price: k.Close})
	if unconfirmed {
		e.emit(Event{Type: "skip", Time: k.OpenTime, Side: sideStr(side), Setup: setup, Reason: "choch_unconfirmed"})
		return strategy.StepResult{}
	}
	return e.proposeEntry(k, side, setup, broken, atrVal, atrReady)
}

// proposeEntry runs the entry gates (signal/direction filters, sweep, HTF, FVG,
// stop construction and sanity filters) for an already-classified structure
// break and produces the proposal (or resting limit). trigger is the broken
// level — the break-even milestone. Shared by both trend models.
func (e *Engine) proposeEntry(k kline.Kline, side strategy.Side, setup string, trigger float64, atrVal float64, atrReady bool) strategy.StepResult {
	skip := func(reason string) strategy.StepResult {
		e.emit(Event{Type: "skip", Time: k.OpenTime, Side: sideStr(side), Setup: setup, Reason: reason})
		return strategy.StepResult{}
	}
	if !e.cfg.tradeable(setup) {
		return skip("signal_filter")
	}
	if e.cfg.LongOnly && side == strategy.Short {
		return skip("long_only") // spot mode: no shorts
	}
	if e.inBlackout(k.CloseTime) {
		return skip("session_blackout")
	}

	// Liquidity-sweep pre-filter: the break must follow a recent grab of the
	// opposite liquidity (a swept low before a long, a swept high before a short).
	if e.cfg.RequireSweep {
		last := e.lastLongSweepBar
		if side == strategy.Short {
			last = e.lastShortSweepBar
		}
		if e.bar-last > e.cfg.SweepLookback {
			return skip("no_recent_sweep")
		}
	}
	// HTF alignment (loose): skip only entries taken directly against the
	// higher-timeframe trend; a flat/unknown HTF trend (0) blocks nothing.
	if e.cfg.HTFAlign {
		switch t := e.htfTracker.Trend(); {
		case side == strategy.Long && t < 0:
			return skip("against_htf_trend")
		case side == strategy.Short && t > 0:
			return skip("against_htf_trend")
		}
	}

	// Entry price: the break close (market), or the FVG level (limit on retrace).
	entry := k.Close
	var fvgLo, fvgHi float64
	var fvgT time.Time
	if e.cfg.UseFVG {
		lvl, z, ok := e.detectFVG(side)
		if !ok {
			return skip("no_fvg") // no imbalance to enter on → skip this break
		}
		entry, fvgLo, fvgHi, fvgT = lvl, z.lo, z.hi, z.formedT
	}

	buffer := e.cfg.StopBufferTicks * e.cfg.TickSize
	var stop, stopDist float64
	switch {
	case e.cfg.FVGStop && e.cfg.UseFVG && fvgHi > fvgLo:
		// Dynamic FVG stop: just beyond the gap — price filling the entire
		// imbalance invalidates the setup, no swing needed.
		buf := buffer
		if atrReady {
			buf = e.cfg.FVGStopBufATR * atrVal
		}
		if side == strategy.Long {
			stop = fvgLo - buf
			stopDist = entry - stop
		} else {
			stop = fvgHi + buf
			stopDist = stop - entry
		}
	case side == strategy.Long:
		if !e.haveSwingLow {
			return skip("no_protective_swing")
		}
		stop = e.swingLow - buffer
		stopDist = entry - stop
	default:
		if !e.haveSwingHigh {
			return skip("no_protective_swing")
		}
		stop = e.swingHigh + buffer
		stopDist = stop - entry
	}
	if stopDist <= 0 {
		return skip("invalid_stop")
	}
	if e.cfg.MaxStopATR > 0 && (!atrReady || stopDist > e.cfg.MaxStopATR*atrVal) {
		return skip("stop_too_wide")
	}
	// Skip stops too tight to trade at sane leverage (and which inflate fees-in-R).
	if e.cfg.MinStopFrac > 0 && stopDist < e.cfg.MinStopFrac*entry {
		return skip("stop_too_tight")
	}

	var target float64
	if side == strategy.Long {
		target = entry + e.cfg.TargetR*stopDist
	} else {
		target = entry - e.cfg.TargetR*stopDist
	}

	feats := e.features(k, side, setup, entry, stopDist, fvgLo, fvgHi, atrVal)

	// FVG mode: rest a limit; the trade is proposed only when/if it fills.
	if e.cfg.UseFVG {
		e.limit = &restingLimit{
			feats: feats,
			side:  side, setup: setup, level: entry, stop: stop, target: target,
			stopDist: stopDist, trigger: trigger, atr: atrVal, barsLeft: e.cfg.FVGMaxWaitBars,
		}
		e.emit(Event{
			Type: "fvg_limit", Time: k.OpenTime, Time2: fvgT,
			Side: sideStr(side), Setup: setup, Level: entry, Lo: fvgLo, Hi: fvgHi,
			Stop: stop, Target: target,
		})
		return strategy.StepResult{}
	}

	e.pending = &pendingEntry{
		feats: feats,
		side:  side, setup: setup, time: k.CloseTime, entry: entry,
		stop: stop, target: target, stopDist: stopDist, atr: atrVal,
	}
	return strategy.StepResult{Proposal: &strategy.Proposal{
		Symbol: e.cfg.Symbol, Side: side, Time: k.CloseTime,
		Entry: entry, Stop: stop, Target: target, StopDist: stopDist,
	}}
}

// trackFVGs maintains the running list of unmitigated fair value gaps: drops
// any gap price has fully retraced through, then records a gap completing on
// this candle (3-candle imbalance, both directions).
func (e *Engine) trackFVGs(k kline.Kline) {
	keep := e.fvgs[:0]
	for _, z := range e.fvgs {
		mitigated := (z.side == strategy.Long && k.Low <= z.lo) ||
			(z.side == strategy.Short && k.High >= z.hi)
		if !mitigated {
			keep = append(keep, z)
		}
	}
	e.fvgs = keep
	n := len(e.buf)
	if n < 3 {
		return
	}
	for _, side := range []strategy.Side{strategy.Long, strategy.Short} {
		if lo, hi, ok := FVG(e.buf[n-3], e.buf[n-1], side); ok {
			e.fvgs = append(e.fvgs, fvgZone{side: side, lo: lo, hi: hi, born: e.bar, formedT: e.buf[n-3].OpenTime})
		}
	}
	if len(e.fvgs) > 8 {
		e.fvgs = e.fvgs[len(e.fvgs)-8:]
	}
}

// detectFVG returns the newest unmitigated fair value gap in the break's
// direction formed within FVGLookback bars (1 = only a gap completing on the
// break candle — the original behavior). The entry level is the near edge
// price first touches on a retrace — the gap top for a long, the gap bottom
// for a short — or the midpoint.
func (e *Engine) detectFVG(side strategy.Side) (level float64, z fvgZone, ok bool) {
	// A paper-thin gap is candle-boundary noise, not an imbalance: require at
	// least FVGMinATR × ATR of height (0 = accept any gap).
	minGap := 0.0
	if e.cfg.FVGMinATR > 0 && e.lastATRReady {
		minGap = e.cfg.FVGMinATR * e.lastATR
	}
	for i := len(e.fvgs) - 1; i >= 0; i-- {
		cand := e.fvgs[i]
		if cand.side != side || e.bar-cand.born >= e.cfg.FVGLookback {
			continue
		}
		if cand.hi-cand.lo < minGap {
			continue
		}
		if e.cfg.FVGMidpoint {
			level = (cand.lo + cand.hi) / 2
		} else if side == strategy.Long {
			level = cand.hi
		} else {
			level = cand.lo
		}
		return level, cand, true
	}
	return 0, fvgZone{}, false
}

// Resolve commits or abandons a pending entry (portfolio decision).
func (e *Engine) Resolve(accept bool) {
	if e.pending == nil {
		return
	}
	pe := e.pending
	e.pending = nil
	if !accept {
		e.emit(Event{Type: "skip", Time: e.lastK.OpenTime, Side: sideStr(pe.side), Setup: pe.setup, Reason: "portfolio_rejected"})
		return
	}
	e.emit(Event{
		Type: "entry", Time: e.lastK.OpenTime, Side: sideStr(pe.side), Setup: pe.setup,
		Price: pe.entry, Stop: pe.stop, Target: pe.target,
	})
	e.pos = &position{
		feats: pe.feats,
		side:  pe.side, setup: pe.setup, entryTime: pe.time, entry: pe.entry,
		stop: pe.stop, target: pe.target, stopDist: pe.stopDist, trigger: pe.trigger,
		atrAtEntry: pe.atr, bestLow: pe.entry, bestHigh: pe.entry, maker: pe.maker,
		remaining: 1,
	}
}

// managePosition checks the stop first (using its value coming into the candle,
// so a trailing stop never uses this bar's own favorable extreme), then trails
// or checks the fixed target.
func (e *Engine) managePosition(k kline.Kline) *strategy.Trade {
	p := e.pos
	if e.cfg.ScaleOut {
		return e.manageScaled(k)
	}
	trailing := e.cfg.TrailATR > 0
	buffer := e.cfg.StopBufferTicks * e.cfg.TickSize
	if p.side == strategy.Long {
		if k.Low <= p.stop {
			return e.close(p.stop, k.CloseTime, e.exitOutcome(trailing))
		}
		e.takePartial(k)
		e.rTrail(k)
		// Structure trail: a new BOS (close above the latest swing high) moves
		// the stop up to the latest swing low (the new higher low).
		if e.cfg.MoveStopOnBOS && e.haveSwingHigh && !e.swingHighBroken && k.Close > e.swingHigh {
			e.swingHighBroken = true
			if ns := e.swingLow - buffer; ns > p.stop {
				e.emit(Event{Type: "stop_move", Time: k.OpenTime, Side: "long", Level: p.stop, Price: ns, Reason: "bos_trail"})
				p.stop = ns
			}
		}
		// Break-even: once price reaches the prev high, move the stop to entry.
		// Checked after the stop (next bars enforce the BEP stop) and applies
		// alongside the fixed target.
		if e.cfg.BreakEven && !p.beArmed && k.High >= p.trigger {
			e.emit(Event{Type: "stop_move", Time: k.OpenTime, Side: "long", Level: p.stop, Price: p.entry, Reason: "break_even"})
			p.stop = p.entry
			p.beArmed = true
		}
		if trailing {
			if k.High > p.bestHigh {
				p.bestHigh = k.High
			}
			if cand := p.bestHigh - e.cfg.TrailATR*p.atrAtEntry; cand > p.stop {
				p.stop = cand
			}
		} else if !e.cfg.RTrail && k.High >= p.target {
			return e.close(p.target, k.CloseTime, strategy.OutcomeTarget)
		}
	} else {
		if k.High >= p.stop {
			return e.close(p.stop, k.CloseTime, e.exitOutcome(trailing))
		}
		e.takePartial(k)
		e.rTrail(k)
		if e.cfg.MoveStopOnBOS && e.haveSwingLow && !e.swingLowBroken && k.Close < e.swingLow {
			e.swingLowBroken = true
			if ns := e.swingHigh + buffer; ns < p.stop {
				e.emit(Event{Type: "stop_move", Time: k.OpenTime, Side: "short", Level: p.stop, Price: ns, Reason: "bos_trail"})
				p.stop = ns
			}
		}
		if e.cfg.BreakEven && !p.beArmed && k.Low <= p.trigger {
			e.emit(Event{Type: "stop_move", Time: k.OpenTime, Side: "short", Level: p.stop, Price: p.entry, Reason: "break_even"})
			p.stop = p.entry
			p.beArmed = true
		}
		if trailing {
			if k.Low < p.bestLow {
				p.bestLow = k.Low
			}
			if cand := p.bestLow + e.cfg.TrailATR*p.atrAtEntry; cand < p.stop {
				p.stop = cand
			}
		} else if !e.cfg.RTrail && k.Low <= p.target {
			return e.close(p.target, k.CloseTime, strategy.OutcomeTarget)
		}
	}
	return nil
}

// rTrail ratchets the stop RTrailOffset behind each R milestone this candle
// reached (RTrailStart, then every RTrailStep), keeping the full position open.
// Checked after the stop, so a raised stop protects subsequent candles only.
func (e *Engine) rTrail(k kline.Kline) {
	if !e.cfg.RTrail {
		return
	}
	p := e.pos
	dir := 1.0
	if p.side == strategy.Short {
		dir = -1
	}
	for {
		m := e.cfg.RTrailStart + float64(p.rtK)*e.cfg.RTrailStep
		lvl := p.entry + dir*m*p.stopDist
		reached := (p.side == strategy.Long && k.High >= lvl) ||
			(p.side == strategy.Short && k.Low <= lvl)
		if !reached {
			return
		}
		ns := p.entry + dir*(m-e.cfg.RTrailOffset)*p.stopDist
		if (p.side == strategy.Long && ns > p.stop) || (p.side == strategy.Short && ns < p.stop) {
			e.emit(Event{Type: "stop_move", Time: k.OpenTime, Side: sideStr(p.side), Level: p.stop, Price: ns, Reason: "r_trail"})
			p.stop = ns
		}
		p.rtK++
	}
}

// exitOutcome tags a stop-hit exit: a trail when an active trailing mode had
// already locked in ground (ATR trail, or an R-trail past its first milestone),
// else a plain stop.
func (e *Engine) exitOutcome(trailing bool) strategy.Outcome {
	if e.cfg.RTrail && e.pos.rtK > 0 {
		return strategy.OutcomeTrail
	}
	return e.stopOutcome(trailing)
}

// takePartial banks the one-shot partial take-profit if this candle reached
// the +PartialAtR milestone: PartialFraction of the position is realized at
// that price and the stop moves to break-even (never loosening a stop that has
// already trailed past entry). Checked after the stop, so the break-even stop
// protects subsequent candles only — no look-ahead.
func (e *Engine) takePartial(k kline.Kline) {
	p := e.pos
	if e.cfg.PartialAtR <= 0 || p.partialDone {
		return
	}
	var lvl float64
	var hit bool
	if p.side == strategy.Long {
		lvl = p.entry + e.cfg.PartialAtR*p.stopDist
		hit = k.High >= lvl
	} else {
		lvl = p.entry - e.cfg.PartialAtR*p.stopDist
		hit = k.Low <= lvl
	}
	if !hit {
		return
	}
	banked := p.remaining * e.cfg.PartialFraction * e.cfg.PartialAtR
	p.realizedR += banked
	p.remaining -= p.remaining * e.cfg.PartialFraction
	p.partialDone = true
	e.emit(Event{Type: "partial", Time: k.OpenTime, Side: sideStr(p.side), Setup: p.setup, Price: lvl, R: banked})
	if (p.side == strategy.Long && p.entry > p.stop) || (p.side == strategy.Short && p.entry < p.stop) {
		e.emit(Event{Type: "stop_move", Time: k.OpenTime, Side: sideStr(p.side), Level: p.stop, Price: p.entry, Reason: "partial_bep"})
		p.stop = p.entry
	}
}

func (e *Engine) stopOutcome(trailing bool) strategy.Outcome {
	if trailing {
		return strategy.OutcomeTrail
	}
	return strategy.OutcomeStop
}

func (e *Engine) close(exit float64, t time.Time, outcome strategy.Outcome) *strategy.Trade {
	p := e.pos
	var r float64
	if p.stopDist > 0 {
		var exitR float64
		if p.side == strategy.Long {
			exitR = (exit - p.entry) / p.stopDist
		} else {
			exitR = (p.entry - exit) / p.stopDist
		}
		// Blend any banked partial with the runner's exit (remaining=1,
		// realizedR=0 when no partial was taken → plain exitR).
		r = p.realizedR + p.remaining*exitR
	}
	tr := &strategy.Trade{
		Symbol: e.cfg.Symbol, Side: p.side, Setup: p.setup,
		EntryTime: p.entryTime, EntryPrice: p.entry,
		Stop: p.stop, Target: p.target, StopDist: p.stopDist,
		ExitTime: t, ExitPrice: exit, Outcome: outcome,
		R: r, ATRAtEntry: p.atrAtEntry, MakerEntry: p.maker,
		Trigger: p.trigger, BEPArmed: p.beArmed, Features: p.feats,
	}
	e.emit(Event{Type: "exit", Time: e.lastK.OpenTime, Side: sideStr(p.side), Setup: p.setup, Price: exit, R: r, Reason: string(outcome)})
	e.pos = nil
	return tr
}

// manageScaled runs the laddered exit: check the stop coming into the candle
// (exiting the runner if hit), then bank ScaleFraction of the remaining position
// at each ScaleStepR milestone this candle reached, ratcheting the stop one step
// behind. A milestone raised this candle only protects subsequent candles (the
// stop was already checked against this candle's extreme), so there is no
// look-ahead. Multiple milestones in one candle are handled in the loop.
func (e *Engine) manageScaled(k kline.Kline) *strategy.Trade {
	p := e.pos
	step := e.cfg.ScaleStepR
	frac := e.cfg.ScaleFraction

	dir := 1.0
	if p.side == strategy.Short {
		dir = -1
	}
	stopHit := (p.side == strategy.Long && k.Low <= p.stop) ||
		(p.side == strategy.Short && k.High >= p.stop)
	if stopHit {
		return e.closeScaled(p.stop, k.CloseTime)
	}
	for {
		nextR := float64(p.scaleK+1) * step
		lvl := p.entry + dir*nextR*p.stopDist
		reached := (p.side == strategy.Long && k.High >= lvl) ||
			(p.side == strategy.Short && k.Low <= lvl)
		if !reached {
			break
		}
		// The cap milestone closes the entire remainder — the ladder's end.
		if e.cfg.ScaleMaxR > 0 && nextR >= e.cfg.ScaleMaxR {
			exit := p.entry + dir*e.cfg.ScaleMaxR*p.stopDist
			p.scaleK++
			return e.closeScaledAs(exit, k.CloseTime, strategy.OutcomeTarget)
		}
		banked := p.remaining * frac * nextR
		p.realizedR += banked
		p.remaining -= p.remaining * frac
		p.scaleK++
		e.emit(Event{Type: "partial", Time: k.OpenTime, Side: sideStr(p.side), Setup: p.setup, Price: lvl, R: banked})
		// Stop trails ScaleTrailR behind the milestone (ratchet only).
		ns := p.entry + dir*(nextR-e.cfg.ScaleTrailR)*p.stopDist
		if (p.side == strategy.Long && ns > p.stop) || (p.side == strategy.Short && ns < p.stop) {
			e.emit(Event{Type: "stop_move", Time: k.OpenTime, Side: sideStr(p.side), Level: p.stop, Price: ns, Reason: "scale_out"})
			p.stop = ns
		}
	}
	return nil
}

// closeScaled exits the remaining runner at exit and blends it with the R already
// banked from the partial exits. The outcome is a stop only if price was stopped
// before any scale-out (a full -1R loss); otherwise the trade locked in profit
// via the ladder, tagged as a trailing exit.
func (e *Engine) closeScaled(exit float64, t time.Time) *strategy.Trade {
	outcome := strategy.OutcomeTrail
	if e.pos.scaleK == 0 {
		outcome = strategy.OutcomeStop
	}
	return e.closeScaledAs(exit, t, outcome)
}

// closeScaledAs is closeScaled with an explicit outcome (the ScaleMaxR cap
// close reports as a target).
func (e *Engine) closeScaledAs(exit float64, t time.Time, outcome strategy.Outcome) *strategy.Trade {
	p := e.pos
	var exitR float64
	if p.stopDist > 0 {
		if p.side == strategy.Long {
			exitR = (exit - p.entry) / p.stopDist
		} else {
			exitR = (p.entry - exit) / p.stopDist
		}
	}
	tr := &strategy.Trade{
		Symbol: e.cfg.Symbol, Side: p.side, Setup: p.setup,
		EntryTime: p.entryTime, EntryPrice: p.entry,
		Stop: p.stop, Target: p.target, StopDist: p.stopDist,
		ExitTime: t, ExitPrice: exit, Outcome: outcome,
		R: p.realizedR + p.remaining*exitR, ATRAtEntry: p.atrAtEntry, MakerEntry: p.maker,
		Trigger: p.trigger, BEPArmed: p.beArmed, Features: p.feats,
	}
	e.emit(Event{Type: "exit", Time: e.lastK.OpenTime, Side: sideStr(p.side), Setup: p.setup, Price: exit, R: tr.R, Reason: string(outcome)})
	e.pos = nil
	return tr
}
