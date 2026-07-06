// Package portfolio is the multi-symbol risk layer. Correlated alts fail-break
// together, so simultaneous signals are one correlated bet, not diversification
// (CLAUDE.md). The Manager caps how many positions and how much total risk can
// be open at once, and when more signals fire than there is room for, it ranks
// them and takes only the best.
package portfolio

import (
	"sort"

	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

// Config sets the portfolio risk caps and account sizing.
type Config struct {
	Capital       float64 // starting capital
	RiskPerTrade  float64 // fraction of capital risked per trade (sizing); default 0.01
	MaxConcurrent int     // max simultaneous open positions; default 5
	MaxTotalRisk  float64 // max total open risk as a fraction of capital; default 0.05
}

func (c *Config) withDefaults() {
	if c.RiskPerTrade <= 0 {
		c.RiskPerTrade = 0.01
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 5
	}
	if c.MaxTotalRisk <= 0 {
		c.MaxTotalRisk = 0.05
	}
}

// Manager tracks open positions and admits new entries within the caps. Each
// position risks exactly RiskPerTrade of capital (by position sizing), so open
// risk is simply openCount × RiskPerTrade.
type Manager struct {
	cfg    Config
	open   map[string]struct{} // symbols currently holding a position
	trades []*strategy.Trade   // all closed trades, in close order
}

// New builds a Manager.
func New(cfg Config) *Manager {
	cfg.withDefaults()
	return &Manager{cfg: cfg, open: make(map[string]struct{})}
}

// Settle records a closed trade, freeing its slot. nil is ignored.
func (m *Manager) Settle(t *strategy.Trade) {
	if t == nil {
		return
	}
	delete(m.open, t.Symbol)
	m.trades = append(m.trades, t)
}

// Admit decides which of the proposals competing on one bar to accept. It ranks
// them tightest-relative-stop first (best reward-to-noise, smallest absolute
// risk per unit) and accepts down the list until either the concurrent-position
// cap or the total-open-risk cap is reached. Accepted proposals reserve a slot
// immediately. The result maps symbol → accepted.
func (m *Manager) Admit(proposals []*strategy.Proposal) map[string]bool {
	decision := make(map[string]bool, len(proposals))
	if len(proposals) == 0 {
		return decision
	}

	ranked := make([]*strategy.Proposal, len(proposals))
	copy(ranked, proposals)
	sort.SliceStable(ranked, func(i, j int) bool {
		return relStop(ranked[i]) < relStop(ranked[j])
	})

	// Risk cap expressed as a position count: floor(MaxTotalRisk/RiskPerTrade).
	riskSlotCap := int(m.cfg.MaxTotalRisk/m.cfg.RiskPerTrade + 1e-9)
	slotCap := m.cfg.MaxConcurrent
	if riskSlotCap < slotCap {
		slotCap = riskSlotCap
	}

	for _, p := range ranked {
		if len(m.open) >= slotCap {
			decision[p.Symbol] = false
			continue
		}
		m.open[p.Symbol] = struct{}{}
		decision[p.Symbol] = true
	}
	return decision
}

// relStop is the stop distance relative to entry price — comparable across
// symbols at different price levels (an absolute stop distance is not).
func relStop(p *strategy.Proposal) float64 {
	if p.Entry == 0 {
		return p.StopDist
	}
	return p.StopDist / p.Entry
}

// OpenCount is the number of positions currently open.
func (m *Manager) OpenCount() int { return len(m.open) }

// Trades returns all closed trades recorded so far.
func (m *Manager) Trades() []*strategy.Trade { return m.trades }
