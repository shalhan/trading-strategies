// Package indicator holds streaming technical indicators fed one candle at a
// time, so backtest and live compute identically.
package indicator

import (
	"math"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

// ATR is a streaming Average True Range using Wilder's smoothing. It is fed
// every closed 5m candle and is continuous across days (volatility does not
// reset at the session boundary). The strategy uses it to size the
// max-stop-distance filter (MAX_STOP_ATR × ATR), which is ATR-based rather than
// a fixed % so it behaves consistently across coins of different volatility.
type ATR struct {
	period  int
	prevClose float64
	hasPrev bool

	seeded  bool    // true once the initial `period` true ranges are averaged
	count   int     // true ranges accumulated during seeding
	sum     float64 // sum of true ranges during seeding
	value   float64 // current ATR once seeded
}

// NewATR creates an ATR with the given period (e.g. 14). It panics on period<1.
func NewATR(period int) *ATR {
	if period < 1 {
		panic("indicator: ATR period must be >= 1")
	}
	return &ATR{period: period}
}

// Update feeds the next closed candle and returns the current ATR and whether
// it is ready (seeded). Before seeding, ready is false and the value is unusable.
func (a *ATR) Update(k kline.Kline) (value float64, ready bool) {
	tr := k.High - k.Low
	if a.hasPrev {
		tr = math.Max(tr, math.Abs(k.High-a.prevClose))
		tr = math.Max(tr, math.Abs(k.Low-a.prevClose))
	}
	a.prevClose = k.Close
	a.hasPrev = true

	if !a.seeded {
		a.sum += tr
		a.count++
		if a.count == a.period {
			a.value = a.sum / float64(a.period)
			a.seeded = true
		}
		return a.value, a.seeded
	}
	// Wilder smoothing: ATR = (prevATR*(n-1) + TR) / n
	a.value = (a.value*float64(a.period-1) + tr) / float64(a.period)
	return a.value, true
}

// Value returns the current ATR and whether it is ready.
func (a *ATR) Value() (float64, bool) { return a.value, a.seeded }
