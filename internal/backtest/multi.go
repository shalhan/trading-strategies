package backtest

import (
	"sort"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/portfolio"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

// Engine is the per-symbol signal generator a portfolio backtest drives. Both
// the failed-break (strategy) and market-structure engines implement it, so
// RunPortfolio is signal-agnostic.
type Engine interface {
	Step(kline.Kline) strategy.StepResult
	Resolve(accept bool)
}

// SymbolData pairs an engine with its kline series for a portfolio backtest.
type SymbolData struct {
	Engine Engine
	Klines []kline.Kline
}

// Gate is an optional market-wide admission filter: a proposal is eligible only
// if Gate returns true. Used for a regime filter (e.g. only trade when BTC is
// trending). nil means no gate.
type Gate func(*strategy.Proposal) bool

// RunPortfolio replays many symbols on one shared timeline through the portfolio
// risk layer, with no market-wide gate.
func RunPortfolio(data map[string]SymbolData, mgr *portfolio.Manager) []*strategy.Trade {
	return RunPortfolioGated(data, mgr, nil)
}

// RunPortfolioGated is RunPortfolio with an optional market-regime gate applied
// before the risk caps: proposals the gate rejects are stood down immediately
// and never compete for a slot.
func RunPortfolioGated(data map[string]SymbolData, mgr *portfolio.Manager, gate Gate) []*strategy.Trade {
	// Flatten into a single (time, symbol) stream and sort by time, then symbol
	// for deterministic intra-bar stepping order.
	type tick struct {
		t   time.Time
		sym string
		k   kline.Kline
	}
	var ticks []tick
	for sym, d := range data {
		for _, k := range d.Klines {
			ticks = append(ticks, tick{t: k.OpenTime, sym: sym, k: k})
		}
	}
	sort.Slice(ticks, func(i, j int) bool {
		if !ticks[i].t.Equal(ticks[j].t) {
			return ticks[i].t.Before(ticks[j].t)
		}
		return ticks[i].sym < ticks[j].sym
	})

	for i := 0; i < len(ticks); {
		// Gather one bar (all ticks sharing the same open time).
		j := i
		var closes []*strategy.Trade
		var proposals []*strategy.Proposal
		propEngine := make(map[string]Engine)

		for j < len(ticks) && ticks[j].t.Equal(ticks[i].t) {
			e := data[ticks[j].sym].Engine
			res := e.Step(ticks[j].k)
			if res.Closed != nil {
				closes = append(closes, res.Closed)
			}
			if res.Proposal != nil {
				proposals = append(proposals, res.Proposal)
				propEngine[res.Proposal.Symbol] = e
			}
			j++
		}

		// Settle exits first so their freed slots are available this bar.
		for _, c := range closes {
			mgr.Settle(c)
		}
		// Market-regime gate: stand down rejected proposals before the caps.
		if gate != nil {
			kept := proposals[:0]
			for _, p := range proposals {
				if gate(p) {
					kept = append(kept, p)
				} else {
					propEngine[p.Symbol].Resolve(false)
					delete(propEngine, p.Symbol)
				}
			}
			proposals = kept
		}
		// Rank and admit the bar's surviving entries, then commit each outcome.
		if len(proposals) > 0 {
			decision := mgr.Admit(proposals)
			for sym, e := range propEngine {
				e.Resolve(decision[sym])
			}
		}
		i = j
	}
	return mgr.Trades()
}
