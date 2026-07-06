package indicator

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
		t.Fatalf("load NY loc: %v", err)
	}
	return loc
}

// vk builds a candle opening at the given NY wall-clock time on 2026-01-02.
func vk(loc *time.Location, hour, min int, h, l, c, vol float64) kline.Kline {
	return kline.Kline{
		OpenTime: time.Date(2026, 1, 2, hour, min, 0, 0, loc),
		High:     h, Low: l, Close: c, Volume: vol,
	}
}

func TestSessionVWAPAccumulates(t *testing.T) {
	loc := nyLoc(t)
	v := NewSessionVWAP(loc)

	if _, ready := v.Value(); ready {
		t.Fatal("ready before any candle")
	}

	// typical = (H+L+C)/3. First candle: typical=10, vol=100 → VWAP=10.
	got, ready := v.Update(vk(loc, 0, 0, 11, 9, 10, 100))
	if !ready || math.Abs(got-10) > 1e-9 {
		t.Fatalf("after 1 candle VWAP=%v ready=%v, want 10", got, ready)
	}

	// Second candle: typical=20, vol=300. cumPV = 10*100 + 20*300 = 7000,
	// cumVol = 400 → VWAP = 17.5.
	got, _ = v.Update(vk(loc, 0, 5, 21, 19, 20, 300))
	if math.Abs(got-17.5) > 1e-9 {
		t.Fatalf("after 2 candles VWAP=%v, want 17.5", got)
	}
}

func TestSessionVWAPResetsOnNewNYDay(t *testing.T) {
	loc := nyLoc(t)
	v := NewSessionVWAP(loc)

	v.Update(vk(loc, 0, 0, 110, 90, 100, 100)) // typical=100
	v.Update(vk(loc, 23, 55, 110, 90, 100, 100))

	// Next candle opens at 00:00 the following NY day → new session, accumulators reset.
	next := vk(loc, 0, 0, 55, 45, 50, 10) // typical=50
	next.OpenTime = next.OpenTime.AddDate(0, 0, 1)
	got, ready := v.Update(next)
	if !ready || math.Abs(got-50) > 1e-9 {
		t.Fatalf("first candle of new session VWAP=%v ready=%v, want 50 (reset)", got, ready)
	}
}

func TestSessionVWAPZeroVolumeCandle(t *testing.T) {
	loc := nyLoc(t)
	v := NewSessionVWAP(loc)

	// A zero-volume candle alone leaves VWAP unready (no volume to weight).
	if _, ready := v.Update(vk(loc, 0, 0, 11, 9, 10, 0)); ready {
		t.Fatal("ready after a zero-volume candle, want not ready")
	}
	// Once a candle brings volume, VWAP is the volume-weighted price.
	got, ready := v.Update(vk(loc, 0, 5, 21, 19, 20, 50))
	if !ready || math.Abs(got-20) > 1e-9 {
		t.Fatalf("VWAP=%v ready=%v, want 20", got, ready)
	}
}
