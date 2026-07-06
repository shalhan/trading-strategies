package main

import (
	"testing"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

func TestSplitPartitionsAtCutoff(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(n int) []kline.Kline {
		ks := make([]kline.Kline, n)
		for i := range ks {
			ks[i] = kline.Kline{OpenTime: base.Add(time.Duration(i) * time.Hour)}
		}
		return ks
	}
	all := map[string]series{
		"A": {tick: 0.01, ks: mk(10)}, // 0..9h
		"B": {tick: 0.10, ks: mk(10)},
	}

	train, test, cutoff := split(all, 0.7) // span 9h → cutoff at +6.3h
	wantCutoff := base.Add(time.Duration(0.7 * float64(9*time.Hour)))
	if !cutoff.Equal(wantCutoff) {
		t.Errorf("cutoff=%v, want %v", cutoff, wantCutoff)
	}

	for sym := range all {
		tr, te := train[sym], test[sym]
		if len(tr.ks)+len(te.ks) != 10 {
			t.Errorf("%s: train+test = %d, want 10", sym, len(tr.ks)+len(te.ks))
		}
		// No overlap and no lookahead: every train candle precedes the cutoff,
		// every test candle is at/after it.
		for _, k := range tr.ks {
			if !k.OpenTime.Before(cutoff) {
				t.Errorf("%s: train candle %v not before cutoff", sym, k.OpenTime)
			}
		}
		for _, k := range te.ks {
			if k.OpenTime.Before(cutoff) {
				t.Errorf("%s: test candle %v before cutoff", sym, k.OpenTime)
			}
		}
		if tr.tick != all[sym].tick {
			t.Errorf("%s: tick size not preserved", sym)
		}
	}
}
