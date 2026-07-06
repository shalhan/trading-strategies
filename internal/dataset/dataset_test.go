package dataset

import (
	"testing"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

func seriesAt(base time.Time, n int) []kline.Kline {
	ks := make([]kline.Kline, n)
	for i := range ks {
		ks[i] = kline.Kline{OpenTime: base.Add(time.Duration(i) * 5 * time.Minute)}
	}
	return ks
}

func TestTrimToWindow(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ks := seriesAt(base, 5) // opens at +0,+5,+10,+15,+20 min

	got := trim(ks, base.Add(5*time.Minute), base.Add(15*time.Minute)) // [+5, +15)
	if len(got) != 2 {
		t.Fatalf("got %d klines, want 2", len(got))
	}
	if !got[0].OpenTime.Equal(base.Add(5*time.Minute)) || !got[1].OpenTime.Equal(base.Add(10*time.Minute)) {
		t.Errorf("trim window wrong: %v, %v", got[0].OpenTime, got[1].OpenTime)
	}
}

func TestCovers(t *testing.T) {
	start := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		firstOpen time.Time
		want      bool
	}{
		{start, true},                          // reaches start exactly
		{start.Add(24 * time.Hour), true},      // within 48h tolerance
		{start.Add(72 * time.Hour), false},     // starts too late → re-fetch
		{start.Add(-24 * time.Hour), true},     // older than start → fine
	}
	for _, c := range cases {
		ks := []kline.Kline{{OpenTime: c.firstOpen}}
		if got := covers(ks, start); got != c.want {
			t.Errorf("covers(first=%v)=%v, want %v", c.firstOpen, got, c.want)
		}
	}
}
