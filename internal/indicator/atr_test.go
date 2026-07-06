package indicator

import (
	"math"
	"testing"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

func k(h, l, c float64) kline.Kline {
	return kline.Kline{High: h, Low: l, Close: c, OpenTime: time.Unix(0, 0)}
}

func TestATRSeedingAndSmoothing(t *testing.T) {
	a := NewATR(3)

	// Not ready until `period` true ranges are seen.
	if _, ready := a.Update(k(10, 9, 9.5)); ready { // TR=1 (no prev close)
		t.Fatal("ready after 1 update, want not ready")
	}
	if _, ready := a.Update(k(11, 10, 10.5)); ready { // TR=max(1,|11-9.5|,|10-9.5|)=1.5
		t.Fatal("ready after 2 updates, want not ready")
	}
	v, ready := a.Update(k(12, 11, 11.5)) // TR=max(1,|12-10.5|,|11-10.5|)=1.5
	if !ready {
		t.Fatal("not ready after 3 updates, want ready")
	}
	// Seed ATR = mean(1, 1.5, 1.5) = 1.333...
	if math.Abs(v-(1+1.5+1.5)/3) > 1e-9 {
		t.Errorf("seed ATR=%v, want %v", v, (1+1.5+1.5)/3)
	}

	// Wilder step: TR = max(1, |13-11.5|, |12-11.5|) = 1.5
	// ATR = (prev*2 + 1.5)/3
	prev := v
	v2, _ := a.Update(k(13, 12, 12.5))
	want := (prev*2 + 1.5) / 3
	if math.Abs(v2-want) > 1e-9 {
		t.Errorf("smoothed ATR=%v, want %v", v2, want)
	}
}
