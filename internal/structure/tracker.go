package structure

import (
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

// Tracker classifies market structure (BOS/CHoCH) at ONE swing scale, set by its
// pivot length, with no trading attached. It is the structure-detection half of
// Engine, exposed for the screener so a caller can report breaks at a chosen
// scale:
//
//   - a LARGE pivot length tracks "swing" structure — the higher-lows and
//     lower-highs that define the trend (the levels a trader circles by hand); a
//     CHoCH here means the trend-defining swing actually broke.
//   - a SMALL pivot length tracks "internal" structure — the minor swings, which
//     flip on shallow pullbacks (noisy on low timeframes).
//
// Running two Trackers at different lengths over the same candles gives the
// two-tier (internal vs swing) view without entangling the trading Engine.
type Tracker struct {
	pivotN int
	buf    []kline.Kline

	haveSwingHigh, haveSwingLow     bool
	swingHigh, swingLow             float64
	swingHighBroken, swingLowBroken bool
	trend                           trendDir
}

// NewTracker builds a structure tracker with the given pivot length (bars on each
// side of a swing). A non-positive length defaults to 3.
func NewTracker(pivotN int) *Tracker {
	if pivotN <= 0 {
		pivotN = 3
	}
	return &Tracker{pivotN: pivotN}
}

// Trend reports the current structural bias: +1 bull, -1 bear, 0 none.
func (t *Tracker) Trend() int {
	switch t.trend {
	case trendBull:
		return 1
	case trendBear:
		return -1
	}
	return 0
}

// SwingHigh / SwingLow return the latest confirmed protective swing levels and
// whether one exists yet — the level a CHoCH would have to break next.
func (t *Tracker) SwingHigh() (float64, bool) { return t.swingHigh, t.haveSwingHigh }
func (t *Tracker) SwingLow() (float64, bool)  { return t.swingLow, t.haveSwingLow }

// Update advances by one candle. It returns a StructureEvent on the candle whose
// close breaks the current swing high (Long) or swing low (Short) — BOS vs CHoCH
// judged against the trend just before this break flips it — and nil otherwise.
// Mirrors Engine.updateSwings + the classification in Engine.tryEnter exactly, so
// the two stay in agreement at the same pivot length.
func (t *Tracker) Update(k kline.Kline) *StructureEvent {
	t.updateSwings(k)
	if t.haveSwingHigh && !t.swingHighBroken && k.Close > t.swingHigh {
		return t.classify(k, strategy.Long)
	}
	if t.haveSwingLow && !t.swingLowBroken && k.Close < t.swingLow {
		return t.classify(k, strategy.Short)
	}
	return nil
}

// updateSwings appends the candle and confirms a pivot pivotN bars back.
func (t *Tracker) updateSwings(k kline.Kline) {
	win := 2*t.pivotN + 1
	t.buf = append(t.buf, k)
	if len(t.buf) > win {
		t.buf = t.buf[1:]
	}
	if len(t.buf) < win {
		return
	}
	c := t.pivotN
	center := t.buf[c]
	isHigh, isLow := true, true
	for i := range t.buf {
		if i == c {
			continue
		}
		if t.buf[i].High >= center.High {
			isHigh = false
		}
		if t.buf[i].Low <= center.Low {
			isLow = false
		}
	}
	if isHigh {
		t.swingHigh, t.haveSwingHigh, t.swingHighBroken = center.High, true, false
	}
	if isLow {
		t.swingLow, t.haveSwingLow, t.swingLowBroken = center.Low, true, false
	}
}

// FVG reports a three-candle fair value gap (imbalance) between c1 and c3 — the
// candles straddling a displacement candle — in the given direction, returning
// the unfilled zone [lo, hi]. A bullish (Long) gap exists when c3.Low > c1.High
// (zone [c1.High, c3.Low]); a bearish (Short) gap when c3.High < c1.Low (zone
// [c3.High, c1.Low]). ok is false when there is no gap. This is the single FVG
// definition shared by the trading engine and the screener.
func FVG(c1, c3 kline.Kline, side strategy.Side) (lo, hi float64, ok bool) {
	if side == strategy.Long {
		if c3.Low > c1.High {
			return c1.High, c3.Low, true
		}
	} else {
		if c3.High < c1.Low {
			return c3.High, c1.Low, true
		}
	}
	return 0, 0, false
}

// classify tags the break BOS/CHoCH, flips the trend, and consumes the level.
func (t *Tracker) classify(k kline.Kline, side strategy.Side) *StructureEvent {
	setup := "BOS"
	level := t.swingHigh // long break clears the swing high
	if side == strategy.Long {
		if t.trend == trendBear {
			setup = "CHoCH"
		}
		t.trend = trendBull
		t.swingHighBroken = true
	} else {
		level = t.swingLow // short break clears the swing low
		if t.trend == trendBull {
			setup = "CHoCH"
		}
		t.trend = trendBear
		t.swingLowBroken = true
	}
	return &StructureEvent{Time: k.CloseTime, Side: side, Setup: setup, Level: level, Price: k.Close}
}
