// Package trendline detects diagonal support/resistance trendlines from swing
// pivots and classifies where price stands relative to them.
//
// A candidate line is drawn through every pair of same-side pivots (highs for
// resistance, lows for support) and validated bar-by-bar: it must never be
// broken by a CLOSE beyond it (wick-overs within tolerance are touches, not
// breaks — matching the repo's confirmation-close convention). A valid line is
// scored by how many distinct touch clusters it has: per the strategy, the best
// trendline touches as many candles as possible without breaking.
//
// One exception to "never broken": a break within the last RecentBreakBars is
// kept and tagged, because a fresh break followed by price returning to the
// line is exactly the breakout-and-retest entry the screener hunts for. A break
// older than that means the line is dead and the candidate is discarded.
//
// Tolerances are in absolute price units; callers derive them from the
// timeframe's ATR (per CLAUDE.md: ATR-based, not fixed %, so behavior is
// consistent across coins of different volatility).
package trendline

import (
	"math"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

// Kind says which side of price the line is drawn on.
type Kind int

const (
	Support    Kind = iota // through pivot lows; price above
	Resistance             // through pivot highs; price below
)

func (k Kind) String() string {
	if k == Resistance {
		return "resistance"
	}
	return "support"
}

// Status classifies current price against a line.
type Status int

const (
	None   Status = iota // price away from the line
	Touch                // price at an unbroken line (potential bounce / break watch)
	Retest               // line was recently broken and price is back at it — the entry setup
)

func (s Status) String() string {
	switch s {
	case Touch:
		return "TOUCH"
	case Retest:
		return "RETEST"
	}
	return "none"
}

// Point is a pivot anchoring a line: bar index and the pivot price
// (high for resistance, low for support).
type Point struct {
	Index int
	Price float64
}

// Line is a validated trendline through anchors A and B (A earlier).
type Line struct {
	Kind      Kind
	A, B      Point
	Slope     float64 // price change per bar
	Touches   int     // distinct touch clusters, anchors included — the quality score
	LastTouch int     // bar index of the most recent touch
	BreakIdx  int     // bar index of the confirming close through the line; -1 if unbroken
}

// ValueAt projects the line's price at a (possibly fractional) bar index, so it
// can be evaluated at "now", past the last closed candle.
func (l Line) ValueAt(idx float64) float64 {
	return l.A.Price + l.Slope*(idx-float64(l.A.Index))
}

// StatusAt classifies price against the line at nowIdx with the given touch
// tolerance, returning the projected line value too.
func (l Line) StatusAt(nowIdx, price, tol float64) (Status, float64) {
	v := l.ValueAt(nowIdx)
	if math.Abs(price-v) > tol {
		return None, v
	}
	if l.BreakIdx >= 0 {
		return Retest, v
	}
	return Touch, v
}

// Config tunes detection. Tolerances are absolute price units (derive from ATR).
type Config struct {
	PivotN          int     // bars each side defining a swing pivot (default 3)
	MinTouches      int     // minimum touch clusters for a line to qualify (default 3)
	MinSpan         int     // minimum bars between the two anchors (default 10)
	TouchTol        float64 // how close an extreme must come to count as a touch
	BreakTol        float64 // how far a close must pass the line to count as a break
	RecentBreakBars int     // keep a broken line only if the break is within this many bars of now (default 12)
}

func (c *Config) withDefaults() {
	if c.PivotN <= 0 {
		c.PivotN = 3
	}
	if c.MinTouches <= 0 {
		c.MinTouches = 3
	}
	if c.MinSpan <= 0 {
		c.MinSpan = 10
	}
	if c.RecentBreakBars <= 0 {
		c.RecentBreakBars = 12
	}
}

// Detect returns the best valid support and resistance lines (0–2 lines) over
// the series: for each side, the candidate with the most touch clusters,
// tie-broken by the most recent touch (the more currently-relevant line).
func Detect(ks []kline.Kline, cfg Config) []Line {
	cfg.withDefaults()
	highs, lows := pivots(ks, cfg.PivotN)

	var out []Line
	if l, ok := bestLine(ks, Resistance, highs, cfg); ok {
		out = append(out, l)
	}
	if l, ok := bestLine(ks, Support, lows, cfg); ok {
		out = append(out, l)
	}
	return out
}

// pivots finds fractal swing points: a high strictly above (low strictly below)
// every neighbor within n bars on each side. Matches the structure engine's
// strict pivot definition.
func pivots(ks []kline.Kline, n int) (highs, lows []Point) {
	for c := n; c < len(ks)-n; c++ {
		isHigh, isLow := true, true
		for i := c - n; i <= c+n; i++ {
			if i == c {
				continue
			}
			if ks[i].High >= ks[c].High {
				isHigh = false
			}
			if ks[i].Low <= ks[c].Low {
				isLow = false
			}
		}
		if isHigh {
			highs = append(highs, Point{Index: c, Price: ks[c].High})
		}
		if isLow {
			lows = append(lows, Point{Index: c, Price: ks[c].Low})
		}
	}
	return highs, lows
}

// bestLine tries every anchor pair at least MinSpan apart and keeps the valid
// line with the most touches (then the freshest last touch).
func bestLine(ks []kline.Kline, kind Kind, pts []Point, cfg Config) (Line, bool) {
	var best Line
	found := false
	for i := 0; i < len(pts); i++ {
		for j := i + 1; j < len(pts); j++ {
			if pts[j].Index-pts[i].Index < cfg.MinSpan {
				continue
			}
			l, ok := evaluate(ks, kind, pts[i], pts[j], cfg)
			if !ok {
				continue
			}
			if !found || l.Touches > best.Touches ||
				(l.Touches == best.Touches && l.LastTouch > best.LastTouch) {
				best, found = l, true
			}
		}
	}
	return best, found
}

// evaluate walks the line from anchor A to the end of the series, counting
// touch clusters and finding the first confirming close through the line.
// Consecutive candles hugging the line count as ONE touch; a gap of at least
// two bars starts a new cluster (so a slow grind along the line doesn't
// outscore a line tested on distinct approaches).
func evaluate(ks []kline.Kline, kind Kind, a, b Point, cfg Config) (Line, bool) {
	slope := (b.Price - a.Price) / float64(b.Index-a.Index)
	l := Line{Kind: kind, A: a, B: b, Slope: slope, BreakIdx: -1}

	prevTouch := -2
	for i := a.Index; i < len(ks); i++ {
		v := a.Price + slope*float64(i-a.Index)
		var broke, touched bool
		if kind == Resistance {
			broke = ks[i].Close > v+cfg.BreakTol
			touched = ks[i].High >= v-cfg.TouchTol
		} else {
			broke = ks[i].Close < v-cfg.BreakTol
			touched = ks[i].Low <= v+cfg.TouchTol
		}
		if broke {
			if i <= b.Index {
				return l, false // not respected between its own anchors
			}
			l.BreakIdx = i
			break // the line's role ends at the breakout
		}
		if touched {
			if i > prevTouch+1 {
				l.Touches++
			}
			prevTouch = i
			l.LastTouch = i
		}
	}

	if l.Touches < cfg.MinTouches {
		return l, false
	}
	if l.BreakIdx >= 0 && (len(ks)-1)-l.BreakIdx > cfg.RecentBreakBars {
		return l, false // broke too long ago — dead line, not a retest setup
	}
	return l, true
}
