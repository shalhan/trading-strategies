package trendline

import (
	"math"
	"testing"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

// syntheticSupport builds a series riding an ascending line y = 100 + 0.5i:
// most candles sit 3 above it, and at each index in touches the low dips
// exactly onto the line (a pivot low, since neighbors sit higher).
func syntheticSupport(n int, touches []int) []kline.Kline {
	touch := make(map[int]bool, len(touches))
	for _, t := range touches {
		touch[t] = true
	}
	ks := make([]kline.Kline, n)
	for i := range ks {
		line := 100 + 0.5*float64(i)
		lo := line + 3
		if touch[i] {
			lo = line
		}
		ks[i] = candle(i, lo+2, lo+4, lo, lo+2)
	}
	return ks
}

func candle(i int, o, h, l, c float64) kline.Kline {
	open := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
	return kline.Kline{OpenTime: open, Open: o, High: h, Low: l, Close: c, CloseTime: open.Add(time.Minute)}
}

func cfg() Config {
	return Config{PivotN: 2, MinTouches: 3, MinSpan: 5, TouchTol: 1, BreakTol: 1, RecentBreakBars: 12}
}

func TestDetectSupportLine(t *testing.T) {
	ks := syntheticSupport(50, []int{5, 15, 25, 35, 45})

	lines := Detect(ks, cfg())
	var sup *Line
	for i := range lines {
		if lines[i].Kind == Support {
			sup = &lines[i]
		}
	}
	if sup == nil {
		t.Fatalf("no support line detected, got %+v", lines)
	}
	if sup.Touches < 4 {
		t.Errorf("touches = %d, want >= 4", sup.Touches)
	}
	if sup.Slope <= 0 {
		t.Errorf("slope = %v, want ascending", sup.Slope)
	}
	if sup.BreakIdx != -1 {
		t.Errorf("BreakIdx = %d, want -1 (unbroken)", sup.BreakIdx)
	}
	// The line should project onto y = 100 + 0.5i.
	if v := sup.ValueAt(40); math.Abs(v-120) > 0.5 {
		t.Errorf("ValueAt(40) = %v, want ~120", v)
	}
}

func TestStatusTouchAndNone(t *testing.T) {
	ks := syntheticSupport(50, []int{5, 15, 25, 35, 45})
	lines := Detect(ks, cfg())
	sup := mustKind(t, lines, Support)

	// Price sitting on the projected line → Touch (line unbroken).
	nowIdx := 50.0
	onLine := sup.ValueAt(nowIdx)
	if st, _ := sup.StatusAt(nowIdx, onLine+0.5, 1); st != Touch {
		t.Errorf("status on line = %v, want Touch", st)
	}
	if st, _ := sup.StatusAt(nowIdx, onLine+10, 1); st != None {
		t.Errorf("status far above line = %v, want None", st)
	}
}

func TestBreakoutRetest(t *testing.T) {
	// Respected support for 40 bars, then a decisive close through it (breakout
	// down), then price pulls back up to the underside of the line (retest).
	ks := syntheticSupport(40, []int{5, 15, 25, 35})
	for i := 40; i < 46; i++ {
		line := 100 + 0.5*float64(i)
		lo := line - 6 // closes well below the line: a confirmed break
		ks = append(ks, candle(i, lo+1, lo+2, lo-1, lo))
	}
	lines := Detect(ks, cfg())
	sup := mustKind(t, lines, Support)

	if sup.BreakIdx < 40 {
		t.Fatalf("BreakIdx = %d, want >= 40 (the breakdown candle)", sup.BreakIdx)
	}
	// Price returns to the line from below → Retest.
	nowIdx := float64(len(ks))
	if st, _ := sup.StatusAt(nowIdx, sup.ValueAt(nowIdx)-0.5, 1); st != Retest {
		t.Errorf("status back at broken line = %v, want Retest", st)
	}
}

func TestStaleBreakDiscarded(t *testing.T) {
	// Same breakout, but followed by a long drift away: the break is older than
	// RecentBreakBars, so the line must be discarded entirely.
	ks := syntheticSupport(40, []int{5, 15, 25, 35})
	for i := 40; i < 70; i++ {
		line := 100 + 0.5*float64(i)
		lo := line - 8
		ks = append(ks, candle(i, lo+1, lo+2, lo-1, lo))
	}
	for _, l := range Detect(ks, cfg()) {
		if l.Kind == Support {
			t.Errorf("stale-broken support line still reported: %+v", l)
		}
	}
}

func TestLineBrokenBetweenAnchorsInvalid(t *testing.T) {
	// A close through the line between its anchors disqualifies the candidate.
	ks := syntheticSupport(50, []int{5, 15, 25, 35, 45})
	line20 := 100 + 0.5*float64(20)
	ks[20] = candle(20, line20, line20+1, line20-4, line20-3) // closes 3 under the line

	lines := Detect(ks, cfg())
	// A support line may still be found (e.g. anchored 25..45 after the break),
	// but no valid line may span the violated candle.
	for _, l := range lines {
		if l.Kind == Support && l.A.Index < 20 && l.B.Index > 20 {
			t.Errorf("line spans a candle that closed through it: %+v", l)
		}
	}
}

func TestDetectResistanceLine(t *testing.T) {
	// Mirror: a descending resistance y = 200 - 0.5i touched at fixed bars.
	touch := map[int]bool{5: true, 15: true, 25: true, 35: true, 45: true}
	ks := make([]kline.Kline, 50)
	for i := range ks {
		line := 200 - 0.5*float64(i)
		hi := line - 3
		if touch[i] {
			hi = line
		}
		ks[i] = candle(i, hi-2, hi, hi-4, hi-2)
	}
	res := mustKind(t, Detect(ks, cfg()), Resistance)
	if res.Touches < 4 {
		t.Errorf("touches = %d, want >= 4", res.Touches)
	}
	if res.Slope >= 0 {
		t.Errorf("slope = %v, want descending", res.Slope)
	}
}

func mustKind(t *testing.T, lines []Line, k Kind) Line {
	t.Helper()
	for _, l := range lines {
		if l.Kind == k {
			return l
		}
	}
	t.Fatalf("no %v line in %+v", k, lines)
	return Line{}
}
