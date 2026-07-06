package backtest

import "github.com/shalhan/orderflow-trading-app/internal/strategy"

// Costs models round-trip trading frictions as fractions applied per side.
//
// The realized R of a trade is reduced by the cost expressed in R. Because 1R in
// price is the stop distance, the cost in R is (cost in price) / stopDist — so
// the *tighter* the stop relative to price, the more a fixed-percentage fee eats
// into each R. That is the crucial effect a gross backtest hides.
type Costs struct {
	FeeRate  float64 // taker fee per side, fraction of notional (e.g. 0.0005 = 0.05%)
	SlipRate float64 // slippage per side, fraction of price  (e.g. 0.0002 = 0.02%)
	MakerFee float64 // maker fee for limit (FVG) entries; no entry slippage. 0 falls back to taker.
}

// Zero reports whether the cost model is a no-op.
func (c Costs) Zero() bool { return c.FeeRate == 0 && c.SlipRate == 0 && c.MakerFee == 0 }

// PerTradeR is the round-trip cost of a trade in R. The exit is always taker
// (market stop/target) and pays slippage. The entry is taker+slippage too,
// UNLESS it was a resting limit (MakerEntry) — then it pays the maker fee and no
// entry slippage (you set the price). Normalized by the stop distance (1R).
func (c Costs) PerTradeR(t *strategy.Trade) float64 {
	if t.StopDist <= 0 {
		return 0
	}
	exitRate := c.FeeRate + c.SlipRate
	entryRate := c.FeeRate + c.SlipRate
	if t.MakerEntry {
		entryRate = c.MakerFee // maker fee, no slippage on a resting limit
	}
	costPrice := entryRate*t.EntryPrice + exitRate*t.ExitPrice
	return costPrice / t.StopDist
}

// Apply returns a copy of the trades with each R reduced by its round-trip cost.
// The originals (gross) are left untouched so both can be reported.
func (c Costs) Apply(trades []*strategy.Trade) []*strategy.Trade {
	if c.Zero() {
		return trades
	}
	out := make([]*strategy.Trade, len(trades))
	for i, t := range trades {
		cp := *t
		cp.R = t.R - c.PerTradeR(t)
		out[i] = &cp
	}
	return out
}

// AvgPerTradeR is the mean round-trip cost across trades, in R (for reporting).
func (c Costs) AvgPerTradeR(trades []*strategy.Trade) float64 {
	if len(trades) == 0 {
		return 0
	}
	var sum float64
	for _, t := range trades {
		sum += c.PerTradeR(t)
	}
	return sum / float64(len(trades))
}
