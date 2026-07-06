package indicator

import (
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

// SessionVWAP is a streaming session-anchored Volume Weighted Average Price.
// It is fed every closed candle and resets at the start of each New York day
// (00:00 ET), consistent with the rest of the bot's session logic — VWAP is an
// intraday reference and is only meaningful relative to a session anchor, never
// a hardcoded UTC offset.
//
// VWAP = Σ(typicalPrice × volume) / Σ(volume), accumulated from the session
// anchor, where typicalPrice = (High + Low + Close) / 3.
type SessionVWAP struct {
	loc *time.Location
	day string // NY date of the current session; a change resets the accumulators

	cumPV  float64 // Σ(typicalPrice × volume) since the anchor
	cumVol float64 // Σ(volume) since the anchor
	value  float64 // current VWAP once any volume has accumulated
	ready  bool
}

// NewSessionVWAP creates a VWAP anchored to the NY day. It panics on a nil loc,
// since every session boundary is computed in that location.
func NewSessionVWAP(loc *time.Location) *SessionVWAP {
	if loc == nil {
		panic("indicator: SessionVWAP requires a non-nil location")
	}
	return &SessionVWAP{loc: loc}
}

// Update feeds the next closed candle and returns the current VWAP and whether
// it is ready. The accumulators reset when the candle crosses into a new NY day,
// so the value always reflects the current session only. A zero-volume candle
// contributes nothing but does not break readiness once the session has volume.
func (v *SessionVWAP) Update(k kline.Kline) (value float64, ready bool) {
	if day := k.NYDate(v.loc); day != v.day {
		v.day = day
		v.cumPV, v.cumVol = 0, 0
		v.value, v.ready = 0, false
	}

	typical := (k.High + k.Low + k.Close) / 3
	v.cumPV += typical * k.Volume
	v.cumVol += k.Volume
	if v.cumVol > 0 {
		v.value = v.cumPV / v.cumVol
		v.ready = true
	}
	return v.value, v.ready
}

// Value returns the current VWAP and whether it is ready (the session has
// accumulated volume).
func (v *SessionVWAP) Value() (float64, bool) { return v.value, v.ready }
