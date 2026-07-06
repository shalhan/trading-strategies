package portfolio

import (
	"testing"

	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

func prop(sym string, entry, stopDist float64) *strategy.Proposal {
	return &strategy.Proposal{Symbol: sym, Entry: entry, StopDist: stopDist}
}

func TestAdmitRanksTightestAndCapsConcurrent(t *testing.T) {
	m := New(Config{Capital: 10000, RiskPerTrade: 0.01, MaxConcurrent: 2, MaxTotalRisk: 0.10})

	// Four signals fire on one bar. Relative stops: A=.05, B=.01, C=.02, D=.03.
	// With MaxConcurrent 2, only the two tightest (B, C) are admitted.
	props := []*strategy.Proposal{
		prop("AUSDT", 100, 5),
		prop("BUSDT", 100, 1),
		prop("CUSDT", 100, 2),
		prop("DUSDT", 100, 3),
	}
	d := m.Admit(props)
	if !d["BUSDT"] || !d["CUSDT"] {
		t.Errorf("tightest two (B,C) should be admitted: %v", d)
	}
	if d["AUSDT"] || d["DUSDT"] {
		t.Errorf("looser signals (A,D) should be rejected: %v", d)
	}
	if m.OpenCount() != 2 {
		t.Errorf("OpenCount=%d, want 2", m.OpenCount())
	}
}

func TestTotalRiskCapBindsBeforeConcurrent(t *testing.T) {
	// MaxConcurrent 5 but total-risk 3% at 1%/trade ⇒ only 3 slots.
	m := New(Config{Capital: 10000, RiskPerTrade: 0.01, MaxConcurrent: 5, MaxTotalRisk: 0.03})
	props := []*strategy.Proposal{
		prop("A", 100, 1), prop("B", 100, 1), prop("C", 100, 1),
		prop("D", 100, 1), prop("E", 100, 1),
	}
	d := m.Admit(props)
	n := 0
	for _, ok := range d {
		if ok {
			n++
		}
	}
	if n != 3 {
		t.Errorf("admitted %d, want 3 (risk cap binds)", n)
	}
}

func TestSettleFreesSlot(t *testing.T) {
	m := New(Config{Capital: 10000, RiskPerTrade: 0.01, MaxConcurrent: 1, MaxTotalRisk: 0.05})

	if d := m.Admit([]*strategy.Proposal{prop("AUSDT", 100, 1)}); !d["AUSDT"] {
		t.Fatal("first proposal should be admitted")
	}
	// Cap of 1 is full: a second symbol is rejected.
	if d := m.Admit([]*strategy.Proposal{prop("BUSDT", 100, 1)}); d["BUSDT"] {
		t.Fatal("second proposal should be rejected while A is open")
	}
	// Close A → slot frees → B can be admitted.
	m.Settle(&strategy.Trade{Symbol: "AUSDT", R: 2})
	if d := m.Admit([]*strategy.Proposal{prop("BUSDT", 100, 1)}); !d["BUSDT"] {
		t.Fatal("after settling A, B should be admitted")
	}
	if got := len(m.Trades()); got != 1 {
		t.Errorf("Trades()=%d, want 1", got)
	}
}
