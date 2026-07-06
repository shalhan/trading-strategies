// Package backtest replays historical klines through the strategy and scores
// the resulting trades. It is pure and deterministic: same klines + config in,
// same trades and stats out.
package backtest

import (
	"math"

	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

// Stats summarizes a set of completed trades, primarily in R-multiples (R is
// risk-normalized so it is comparable across symbols and price levels).
type Stats struct {
	Trades   int
	Wins     int
	Losses   int
	Scratch  int // exactly-flat outcomes (rare; e.g. EOD at entry)
	WinRate  float64

	Targets int
	Stops   int
	EODs    int
	Trails  int

	TotalR      float64 // sum of realized R
	ExpectancyR float64 // mean R per trade
	GrossWinR   float64
	GrossLossR  float64 // negative
	ProfitFactor float64 // grossWin / |grossLoss|; +Inf if no losses

	MaxDrawdownR float64 // largest peak-to-trough drop of the cumulative-R curve

	// Account simulation (fixed-fractional, non-compounding): each trade risks
	// RiskPerTrade × StartCapital, so PnL = R × riskAmount.
	StartCapital float64
	RiskPerTrade float64
	EndEquity    float64
}

// Compute derives Stats from trades. startCapital and riskPerTrade drive the
// account simulation; pass 0 to skip it (equity fields stay zero).
func Compute(trades []*strategy.Trade, startCapital, riskPerTrade float64) Stats {
	s := Stats{StartCapital: startCapital, RiskPerTrade: riskPerTrade}
	riskAmount := startCapital * riskPerTrade

	var cum, peak float64
	equity := startCapital
	for _, t := range trades {
		s.Trades++
		switch {
		case t.R > 0:
			s.Wins++
			s.GrossWinR += t.R
		case t.R < 0:
			s.Losses++
			s.GrossLossR += t.R
		default:
			s.Scratch++
		}
		switch t.Outcome {
		case strategy.OutcomeTarget:
			s.Targets++
		case strategy.OutcomeStop:
			s.Stops++
		case strategy.OutcomeEOD:
			s.EODs++
		case strategy.OutcomeTrail:
			s.Trails++
		}

		cum += t.R
		if cum > peak {
			peak = cum
		}
		if dd := peak - cum; dd > s.MaxDrawdownR {
			s.MaxDrawdownR = dd
		}
		equity += t.R * riskAmount
	}

	s.TotalR = cum
	if s.Trades > 0 {
		s.WinRate = float64(s.Wins) / float64(s.Trades)
		s.ExpectancyR = cum / float64(s.Trades)
	}
	switch {
	case s.GrossLossR == 0 && s.GrossWinR > 0:
		s.ProfitFactor = math.Inf(1)
	case s.GrossLossR != 0:
		s.ProfitFactor = s.GrossWinR / math.Abs(s.GrossLossR)
	}
	s.EndEquity = equity
	return s
}
