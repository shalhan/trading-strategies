// Package kline defines the 5-minute OHLCV candle that v1 trades on, plus the
// New-York-day helpers the opening-range logic depends on.
//
// All session/date logic is anchored to America/New_York (DST-aware) per
// CLAUDE.md — never a hardcoded UTC offset.
package kline

import "time"

// Kline is one closed OHLCV candle. Times are absolute instants; convert to NY
// with the helpers below for any session logic.
type Kline struct {
	OpenTime  time.Time
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	CloseTime time.Time
}

// NYDate returns the calendar date of the candle's open in loc, as "2006-01-02".
// This is the per-day grouping key: the range, attempt caps, and force-close
// all reset when this value changes.
func (k Kline) NYDate(loc *time.Location) string {
	return k.OpenTime.In(loc).Format("2006-01-02")
}

// InOpeningWindow reports whether the candle's open falls inside the first
// 4-hour window of the NY day (00:00–04:00 ET). These candles define the range.
func (k Kline) InOpeningWindow(loc *time.Location) bool {
	return k.OpenTime.In(loc).Hour() < 4
}

// IsNYMidnightOpen reports whether the candle opens exactly at 00:00 ET — the
// first candle of the window. Seeing it confirms we have the full opening
// window (vs. starting mid-day on incomplete data).
func (k Kline) IsNYMidnightOpen(loc *time.Location) bool {
	t := k.OpenTime.In(loc)
	return t.Hour() == 0 && t.Minute() == 0
}
