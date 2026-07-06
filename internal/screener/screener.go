// Package screener scans a set of symbols with the structure engine and scores
// every setup it produces against a transparent, weighted checklist. It is the
// shared core of cmd/smcscan (CLI alerts) and cmd/viz's scan panel, so both see
// exactly the same candidates.
//
// Scoring criteria (each weight configurable, score normalized to 0-100 over
// the criteria that apply):
//
//	HTF       — break direction agrees with the higher-timeframe structure
//	            trend (graded: full / half when flat / zero against)
//	Setup     — CHoCH full points, BOS half
//	FVG       — an unmitigated fair value gap to rest the entry in
//	Stop      — stop ≤2 ATR full, ≤ MaxStopATR half
//	Impulse   — gap ≥0.3 ATR (or break candle ≥1.5 ATR in market-entry mode)
//
// Stages: SETUP (break closed, limit resting), TRIGGER (limit filled —
// actionable), ENTER (market-entry mode: actionable at the break close). The
// scanner never holds positions (it releases every proposal), so detection is
// continuous.
package screener

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/dataset"
	"github.com/shalhan/orderflow-trading-app/internal/indicator"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/structure"
)

// Weights are the checklist points per criterion.
type Weights struct {
	HTF, Setup, FVG, Stop, Impulse int
}

// DefaultWeights reflects what this project has measured so far: HTF
// misalignment was the dominant bleed, CHoCH (leg model) is the rarer signal.
func DefaultWeights() Weights { return Weights{HTF: 30, Setup: 20, FVG: 20, Stop: 15, Impulse: 15} }

// Config tunes a Scanner.
type Config struct {
	Interval  string        // signal timeframe (Binance name, e.g. "4h")
	D         time.Duration // its duration
	Bars      int           // history per symbol
	HTFD      time.Duration // higher timeframe for the alignment score (0 = criterion excluded)
	HTFPivotN int           // swing strength of the HTF trend tracker
	Workers   int           // concurrent symbols per scan
	W         Weights
	Engine    structure.Config // per-symbol engine template (Symbol is overridden)
}

// Candidate is one scored setup.
type Candidate struct {
	Symbol, Stage, Side, Setup string
	Time                       time.Time
	Entry, Stop, Target        float64
	GapLo, GapHi               float64
	Score                      int
	Parts                      string
	Resting                    bool
}

// Scanner holds the persistent per-symbol state; feed it repeatedly (watch
// mode) or once (one-shot).
type Scanner struct {
	cfg    Config
	states map[string]*symState
}

// New builds a scanner over the given symbols.
func New(cfg Config, symbols []string) *Scanner {
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}
	if cfg.Engine.ATRPeriod <= 0 {
		cfg.Engine.ATRPeriod = 14
	}
	sc := &Scanner{cfg: cfg, states: make(map[string]*symState, len(symbols))}
	for _, sym := range symbols {
		sc.states[sym] = newSymState(sym, cfg)
	}
	return sc
}

// Scan fetches fresh klines for every symbol (in parallel) and feeds candles
// not yet seen through the engines, forwarding scored candidates to out. On the
// first call it replays the whole history window; later calls only feed new
// closes, so a watch loop costs ~one request per symbol.
func (sc *Scanner) Scan(ctx context.Context, client *binance.Client, dataDir string, out func(Candidate)) {
	now := time.Now().UTC()
	start := now.Add(-sc.cfg.D * time.Duration(sc.cfg.Bars+sc.cfg.Engine.ATRPeriod+10))
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, sc.cfg.Workers)
	)
	for _, s := range sc.states {
		wg.Add(1)
		sem <- struct{}{}
		go func(s *symState) {
			defer wg.Done()
			defer func() { <-sem }()
			fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			ks, err := dataset.LoadKlines(fctx, client, dataDir, s.sym, sc.cfg.Interval, start, now, true)
			if err != nil {
				return
			}
			for len(ks) > 0 && ks[len(ks)-1].CloseTime.After(now) {
				ks = ks[:len(ks)-1] // drop the still-forming candle
			}
			for _, k := range ks {
				if !k.OpenTime.After(s.lastFed) {
					continue
				}
				s.feed(k, sc.cfg, func(cand Candidate) {
					mu.Lock()
					out(cand)
					mu.Unlock()
				})
			}
		}(s)
	}
	wg.Wait()
}

// Resting returns the limits currently awaiting a fill — the live opportunities.
func (sc *Scanner) Resting() []Candidate {
	var out []Candidate
	for _, s := range sc.states {
		for _, p := range s.pending {
			if p != nil && p.Resting {
				out = append(out, *p)
			}
		}
	}
	return out
}

// symState is the persistent per-symbol scanner state.
type symState struct {
	sym     string
	eng     *structure.Engine
	atr     *indicator.ATR
	atrVal  float64
	atrOK   bool
	htf     htfAgg
	lastFed time.Time
	events  []structure.Event
	pending map[string]*Candidate // side -> resting SETUP awaiting fill
}

func newSymState(sym string, c Config) *symState {
	ecfg := c.Engine
	ecfg.Symbol = sym
	s := &symState{sym: sym, eng: structure.New(ecfg), atr: indicator.NewATR(c.Engine.ATRPeriod), pending: map[string]*Candidate{}}
	s.eng.SetEventHook(func(ev structure.Event) { s.events = append(s.events, ev) })
	if c.HTFD > 0 {
		s.htf = htfAgg{tr: structure.NewTracker(c.HTFPivotN), d: c.HTFD}
	}
	return s
}

// feed advances one symbol by one candle and emits any scored candidates.
func (s *symState) feed(k kline.Kline, c Config, out func(Candidate)) {
	s.htf.update(k)
	s.atrVal, s.atrOK = s.atr.Update(k)
	s.events = s.events[:0]
	res := s.eng.Step(k)
	if res.Proposal != nil {
		s.eng.Resolve(false) // never hold a position: keep detecting
	}
	s.lastFed = k.OpenTime

	for _, ev := range s.events {
		switch ev.Type {
		case "fvg_limit":
			cand := Candidate{
				Symbol: s.sym, Stage: "SETUP", Side: ev.Side, Setup: ev.Setup, Time: k.OpenTime,
				Entry: ev.Level, Stop: ev.Stop, Target: ev.Target, GapLo: ev.Lo, GapHi: ev.Hi,
			}
			cand.Score, cand.Parts = score(&cand, c, s.htf.trend(), s.atrVal, s.atrOK, k)
			live := cand
			live.Resting = true
			s.pending[ev.Side] = &live
			out(cand)
		case "limit_filled":
			if p := s.pending[ev.Side]; p != nil {
				trig := *p
				trig.Stage, trig.Time, trig.Resting = "TRIGGER", k.OpenTime, false
				// re-score: the HTF trend may have flipped while waiting
				trig.Score, trig.Parts = score(&trig, c, s.htf.trend(), s.atrVal, s.atrOK, k)
				s.pending[ev.Side] = nil
				out(trig)
			}
		case "limit_cancelled":
			s.pending[ev.Side] = nil
		}
	}
	// Market-entry mode: the proposal itself is the actionable stage.
	if res.Proposal != nil && !c.Engine.UseFVG {
		pr := res.Proposal
		cand := Candidate{
			Symbol: s.sym, Stage: "ENTER", Side: strings.ToLower(pr.Side.String()), Time: k.OpenTime,
			Entry: pr.Entry, Stop: pr.Stop, Target: pr.Target,
		}
		for _, ev := range s.events { // recover the setup tag from the break event
			if ev.Type == "break" {
				cand.Setup = ev.Setup
			}
		}
		cand.Score, cand.Parts = score(&cand, c, s.htf.trend(), s.atrVal, s.atrOK, k)
		out(cand)
	}
}

// score applies the weighted checklist, normalized to 0-100 over the criteria
// that apply to this candidate.
func score(cand *Candidate, c Config, htfTrend int, atrVal float64, atrOK bool, k kline.Kline) (int, string) {
	earned, max := 0, 0
	var parts []string
	add := func(name string, pts, of int) {
		earned += pts
		max += of
		parts = append(parts, fmt.Sprintf("%s+%d", name, pts))
	}

	if c.HTFD > 0 {
		dir := 1
		if cand.Side == "short" {
			dir = -1
		}
		switch {
		case htfTrend == dir:
			add("htf", c.W.HTF, c.W.HTF)
		case htfTrend == 0:
			add("htf~", c.W.HTF/2, c.W.HTF)
		default:
			add("htf", 0, c.W.HTF)
		}
	}

	if cand.Setup == "CHoCH" {
		add("choch", c.W.Setup, c.W.Setup)
	} else {
		add("bos", c.W.Setup/2, c.W.Setup)
	}

	hasGap := cand.GapHi > cand.GapLo
	if c.Engine.UseFVG {
		if hasGap {
			add("fvg", c.W.FVG, c.W.FVG)
		} else {
			add("fvg", 0, c.W.FVG)
		}
	}

	if atrOK && atrVal > 0 {
		dist := cand.Entry - cand.Stop
		if cand.Side == "short" {
			dist = cand.Stop - cand.Entry
		}
		switch {
		case dist <= 2*atrVal:
			add("stop", c.W.Stop, c.W.Stop)
		case c.Engine.MaxStopATR <= 0 || dist <= c.Engine.MaxStopATR*atrVal:
			add("stop", c.W.Stop/2, c.W.Stop)
		default:
			add("stop", 0, c.W.Stop)
		}

		impulse := k.High - k.Low // break candle range (market mode)
		threshold := 1.5 * atrVal
		if hasGap {
			impulse, threshold = cand.GapHi-cand.GapLo, 0.3*atrVal
		}
		if impulse >= threshold {
			add("imp", c.W.Impulse, c.W.Impulse)
		} else {
			add("imp", 0, c.W.Impulse)
		}
	}

	if max == 0 {
		return 0, ""
	}
	return 100 * earned / max, strings.Join(parts, " ")
}

// htfAgg folds primary candles into higher-timeframe buckets and feeds only
// CLOSED buckets to a structure Tracker (same construction as Engine.updateHTF).
type htfAgg struct {
	tr         *structure.Tracker
	d          time.Duration
	started    bool
	bucket     time.Time
	o, h, l, c float64
}

func (a *htfAgg) update(k kline.Kline) {
	if a.tr == nil {
		return
	}
	bucket := k.OpenTime.Truncate(a.d)
	switch {
	case !a.started:
		a.started = true
	case bucket != a.bucket:
		a.tr.Update(kline.Kline{OpenTime: a.bucket, Open: a.o, High: a.h, Low: a.l, Close: a.c, CloseTime: bucket})
	default:
		if k.High > a.h {
			a.h = k.High
		}
		if k.Low < a.l {
			a.l = k.Low
		}
		a.c = k.Close
		return
	}
	a.bucket = bucket
	a.o, a.h, a.l, a.c = k.Open, k.High, k.Low, k.Close
}

func (a *htfAgg) trend() int {
	if a.tr == nil {
		return 0
	}
	return a.tr.Trend()
}
