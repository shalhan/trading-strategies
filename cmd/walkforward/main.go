// Command walkforward stress-tests a fixed strategy config across multiple
// independent time segments and cost assumptions. A single good train/test
// window can be luck; an edge that survives is one that is net-positive across
// most segments and does not evaporate under worse costs.
//
// It does NOT re-tune per segment — the whole point is to see whether ONE fixed
// configuration holds up across regimes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/backtest"
	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/dataset"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/portfolio"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
	"github.com/shalhan/orderflow-trading-app/internal/structure"
	"github.com/shalhan/orderflow-trading-app/internal/universe"
)

type series struct {
	tick float64
	ks   []kline.Kline
}

type params struct {
	pivotN        int
	atrPeriod     int
	maxStopATR    float64
	minStopFrac   float64
	trailATR      float64
	targetR       float64
	signals       string
	longOnly      bool
	useFVG        bool
	fvgMid        bool
	moveStopOnBOS bool
	breakEven     bool
	strictChoch   bool
	trendModel    string
	swingDev      float64
	partialR      float64
	scaleOut      bool
	scaleStep     float64
	scaleFrac     float64
	scaleTrail    float64
	scaleMax      float64
	fvgStop       bool
	fvgStopBuf    float64
	luxLen        int
	fvgLookback   int
	sessions      string
	sessionBuf    int
	rTrail        bool
	rTrailStart   float64
	rTrailStep    float64
	rTrailOff     float64
	liqSweep      bool
	fvgMinATR     float64
	bufferTicks   float64
	capital, risk float64
	maxConcurrent int
}

func main() {
	top := flag.Int("top", 20, "universe size")
	days := flag.Int("days", 365, "total lookback window (ignored if -start set)")
	startStr := flag.String("start", "", "start date YYYY-MM-DD (overrides -days)")
	endStr := flag.String("end", "", "end date YYYY-MM-DD (default: now)")
	segments := flag.Int("segments", 6, "number of sequential walk-forward segments")
	interval := flag.String("interval", "1h", "kline interval")
	dataDir := flag.String("datadir", "./data", "kline cache dir")
	refresh := flag.Bool("refresh", false, "force re-fetch")

	pivotN := flag.Int("pivot-n", 3, "structure swing strength")
	atrPeriod := flag.Int("atr-period", 14, "ATR period")
	maxStopATR := flag.Float64("max-stop-atr", 3, "stop filter")
	minStopPct := flag.Float64("min-stop-pct", 0, "skip trades with stop tighter than this %% of price")
	trailATR := flag.Float64("trail-atr", 0, "trailing stop (0 = fixed target)")
	targetR := flag.Float64("target-r", 2, "fixed R target")
	signals := flag.String("signals", "both", "both | bos | choch")
	longOnly := flag.Bool("long-only", false, "spot mode: long entries only")
	useFVG := flag.Bool("fvg", false, "enter via a limit in the Fair Value Gap on retrace (maker)")
	fvgMid := flag.Bool("fvg-mid", false, "enter at the FVG midpoint instead of its edge")
	fvgLookback := flag.Int("fvg-lookback", 1, "bars back an unmitigated FVG may have formed and still serve a break (1 = break candle only)")
	sessions := flag.String("block-sessions", "", "block entries around session opens/closes: comma list of asia,london,us (empty = off)")
	sessionBuf := flag.Int("session-buf", 30, "session blackout half-width in minutes")
	rTrail := flag.Bool("r-trail", false, "milestone R-trail, full position (replaces target)")
	rTrailStart := flag.Float64("r-trail-start", 2, "first R-trail milestone")
	rTrailStep := flag.Float64("r-trail-step", 1, "R spacing of later milestones")
	rTrailOff := flag.Float64("r-trail-offset", 0.5, "stop trails this many R behind the last milestone")
	moveStopOnBOS := flag.Bool("move-stop-on-bos", false, "trail stop to latest swing on each new BOS")
	strictChoch := flag.Bool("strict-choch", false, "CHoCH only after a confirmed reversal sequence (HL formed + LH broken, or mirror)")
	trendModel := flag.String("trend-model", "pivot", "trend/CHoCH definition: pivot (naive, baseline) | leg (protected-swing SMC) | lux (LuxAlgo SMC)")
	luxLen := flag.Int("lux-len", 50, "lux model: swing confirms when a bar's extreme beats this many bars after it")
	swingDev := flag.Float64("swing-dev", 0, "ZigZag swing detection: min reversal in ATRs to confirm a swing (0 = pivot-n)")
	partialR := flag.Float64("partial-r", 0, "bank 50%% and move stop to break-even at this profit in R (0 = off)")
	scaleOut := flag.Bool("scale-out", false, "laddered exit: bank a fraction at every scale-step-r milestone (replaces target/trails)")
	scaleStep := flag.Float64("scale-step-r", 2, "R spacing between ladder milestones")
	scaleFrac := flag.Float64("scale-fraction", 0.5, "fraction of the remaining position banked at each milestone")
	scaleTrail := flag.Float64("scale-trail-r", 0, "stop trails this many R behind the last milestone (0 = one full step)")
	scaleMax := flag.Float64("scale-max-r", 0, "close the entire remainder at this milestone (0 = uncapped)")
	fvgStop := flag.Bool("fvg-stop", false, "stop just beyond the FVG instead of the protective swing (requires -fvg)")
	fvgStopBuf := flag.Float64("fvg-stop-buf", 0.1, "FVG stop buffer beyond the gap edge, in ATRs")
	breakEven := flag.Bool("break-even", false, "move stop to BEP once price reaches the prev high/low")
	liqSweep := flag.Bool("liq-sweep", false, "liquidity-sweep entry (wick+reject a swing, then high-impact FVG)")
	fvgMinATR := flag.Float64("fvg-min-atr", 0, "min FVG size in ATR units (high-impact filter)")
	bufferTicks := flag.Float64("stop-buffer-ticks", 2, "stop buffer ticks")
	regimeMA := flag.Int("regime-ma", 0, "only trade when BTC close > its N-bar SMA (0 = off)")
	capital := flag.Float64("capital", 10000, "starting capital")
	risk := flag.Float64("risk", 0.01, "risk per trade")
	maxConc := flag.Int("max-concurrent", 5, "max concurrent positions")
	feeRate := flag.Float64("fee-rate", 0.0005, "taker fee per side")
	makerFee := flag.Float64("maker-fee", 0.0002, "maker fee per side for FVG limit entries")
	flag.Parse()

	p := params{
		pivotN: *pivotN, atrPeriod: *atrPeriod, maxStopATR: *maxStopATR, minStopFrac: *minStopPct / 100,
		trailATR: *trailATR, targetR: *targetR, signals: *signals, longOnly: *longOnly,
		useFVG: *useFVG, fvgMid: *fvgMid, moveStopOnBOS: *moveStopOnBOS, breakEven: *breakEven,
		strictChoch: *strictChoch, trendModel: *trendModel, swingDev: *swingDev, partialR: *partialR,
		scaleOut: *scaleOut, scaleStep: *scaleStep, scaleFrac: *scaleFrac, scaleTrail: *scaleTrail, scaleMax: *scaleMax,
		fvgStop: *fvgStop, fvgStopBuf: *fvgStopBuf, luxLen: *luxLen, fvgLookback: *fvgLookback,
		sessions: *sessions, sessionBuf: *sessionBuf,
		rTrail: *rTrail, rTrailStart: *rTrailStart, rTrailStep: *rTrailStep, rTrailOff: *rTrailOff,
		liqSweep: *liqSweep, fvgMinATR: *fvgMinATR,
		bufferTicks: *bufferTicks, capital: *capital, risk: *risk, maxConcurrent: *maxConc,
	}

	startT, endT := resolveRange(*startStr, *endStr, *days)
	all := loadData(*top, startT, endT, *dataDir, *interval, *refresh)
	minT, maxT := bounds(all)
	fmt.Printf("\nloaded %d symbols, %s → %s\n", len(all), minT.Format("2006-01-02"), maxT.Format("2006-01-02"))

	gate := btcRegimeGate(all, *regimeMA)
	if gate != nil {
		fmt.Printf("regime filter ON: only trade when BTC close > %d-bar SMA\n", *regimeMA)
	}

	// --- Walk-forward by sub-period ---
	// The strategy runs ONCE, continuously (full price history, like real
	// trading — never forgetting). Each completed trade is then attributed to
	// the 2-month period it was entered in. No restarts, no warmup penalty, no
	// dropped boundary trades — the segments sum to the real continuous total.
	costs := backtest.Costs{FeeRate: *feeRate, SlipRate: 0.0002, MakerFee: *makerFee}
	netTrades := costs.Apply(runStructureTrades(all, p, gate))

	fmt.Printf("\n=== Walk-forward by period (one continuous run, %d segments) ===\n", *segments)
	fmt.Printf("fixed config: stop %.1f, conc %d, target %gR, %s\n", *maxStopATR, *maxConc, *targetR, *signals)
	fmt.Printf("%-23s %7s %9s %7s %7s\n", "period (by entry)", "trades", "netR", "PF", "win%")
	span := maxT.Sub(minT)
	segLen := span / time.Duration(*segments)
	var posSeg int
	var totalR float64
	for i := 0; i < *segments; i++ {
		segStart := minT.Add(time.Duration(i) * segLen)
		segEnd := segStart.Add(segLen)
		if i == *segments-1 {
			segEnd = maxT.Add(time.Hour) // include the final bar
		}
		var sub []*strategy.Trade
		for _, t := range netTrades {
			if !t.EntryTime.Before(segStart) && t.EntryTime.Before(segEnd) {
				sub = append(sub, t)
			}
		}
		s := backtest.Compute(sub, p.capital, p.risk)
		label := fmt.Sprintf("%s→%s", segStart.Format("01-02"), segEnd.Format("01-02"))
		fmt.Printf("%-23s %7d %+9.2f %7.2f %6.0f%%\n", label, s.Trades, s.TotalR, s.ProfitFactor, s.WinRate*100)
		if s.TotalR > 0 {
			posSeg++
		}
		totalR += s.TotalR
	}
	fmt.Printf("%-23s %7s %+9.2f  (= continuous full-year total)\n", "TOTAL", "", totalR)
	fmt.Printf("\n%d/%d segments net-positive. ", posSeg, *segments)
	switch {
	case posSeg >= *segments-1:
		fmt.Println("Consistent across regimes — the edge looks robust.")
	case posSeg <= *segments/2:
		fmt.Println("Inconsistent — likely regime-dependent, not a reliable edge.")
	default:
		fmt.Println("Mixed — borderline; needs more data before trusting.")
	}

	// --- Cost sensitivity on the full window ---
	fmt.Printf("\n=== Cost sensitivity (full window) ===\n")
	fmt.Printf("%-22s %9s %9s %7s\n", "slippage/side", "netR", "exp", "PF")
	for _, slip := range []float64{0, 0.0002, 0.0005, 0.001} {
		c := backtest.Costs{FeeRate: *feeRate, SlipRate: slip, MakerFee: *makerFee}
		s := runStructure(all, p, c, gate)
		fmt.Printf("%-22s %+9.2f %+9.3f %7.2f\n",
			fmt.Sprintf("%.2f%% (fee %.2f%%)", slip*100, *feeRate*100), s.TotalR, s.ExpectancyR, s.ProfitFactor)
	}
	fmt.Println("\n(fee held at taker; slippage is the uncertain part on alts)")
}

// runStructureTrades runs one continuous portfolio backtest and returns the
// gross completed trades.
func runStructureTrades(data map[string]series, p params, gate backtest.Gate) []*strategy.Trade {
	sd := make(map[string]backtest.SymbolData, len(data))
	for sym, s := range data {
		if len(s.ks) == 0 {
			continue
		}
		eng := structure.New(structure.Config{
			Symbol: sym, PivotN: p.pivotN, ATRPeriod: p.atrPeriod,
			MaxStopATR: p.maxStopATR, MinStopFrac: p.minStopFrac, TrailATR: p.trailATR, TargetR: p.targetR,
			StopBufferTicks: p.bufferTicks, TickSize: s.tick, Signals: p.signals,
			LongOnly: p.longOnly, UseFVG: p.useFVG, FVGMidpoint: p.fvgMid,
			MoveStopOnBOS: p.moveStopOnBOS, BreakEven: p.breakEven, StrictCHoCH: p.strictChoch,
			TrendModel: p.trendModel, SwingDevATR: p.swingDev, PartialAtR: p.partialR,
			ScaleOut: p.scaleOut, ScaleStepR: p.scaleStep, ScaleFraction: p.scaleFrac,
			ScaleTrailR: p.scaleTrail, ScaleMaxR: p.scaleMax,
			FVGStop: p.fvgStop, FVGStopBufATR: p.fvgStopBuf, LuxLen: p.luxLen, FVGLookback: p.fvgLookback,
			BlackoutSessions: p.sessions, SessionBufMin: p.sessionBuf,
			RTrail: p.rTrail, RTrailStart: p.rTrailStart, RTrailStep: p.rTrailStep, RTrailOffset: p.rTrailOff,
			LiquiditySweep: p.liqSweep, FVGMinATR: p.fvgMinATR,
		})
		sd[sym] = backtest.SymbolData{Engine: eng, Klines: s.ks}
	}
	mgr := portfolio.New(portfolio.Config{
		Capital: p.capital, RiskPerTrade: p.risk,
		MaxConcurrent: p.maxConcurrent, MaxTotalRisk: float64(p.maxConcurrent) * p.risk,
	})
	return backtest.RunPortfolioGated(sd, mgr, gate)
}

func runStructure(data map[string]series, p params, costs backtest.Costs, gate backtest.Gate) backtest.Stats {
	return backtest.Compute(costs.Apply(runStructureTrades(data, p, gate)), p.capital, p.risk)
}

// btcRegimeGate builds a market-regime filter from BTC: a proposal is allowed
// only if BTC's close was above its maPeriod-bar SMA as of the proposal time.
// Returns nil if disabled or BTC data is unavailable.
func btcRegimeGate(all map[string]series, maPeriod int) backtest.Gate {
	if maPeriod <= 0 {
		return nil
	}
	btc, ok := all["BTCUSDT"]
	if !ok || len(btc.ks) < maPeriod {
		fmt.Println("regime filter requested but BTCUSDT data unavailable — running without it")
		return nil
	}
	type pt struct {
		t  int64
		on bool
	}
	var pts []pt
	var sum float64
	for i, k := range btc.ks {
		sum += k.Close
		if i >= maPeriod {
			sum -= btc.ks[i-maPeriod].Close
		}
		if i >= maPeriod-1 {
			ma := sum / float64(maPeriod)
			pts = append(pts, pt{t: k.CloseTime.UnixMilli(), on: k.Close > ma})
		}
	}
	return func(p *strategy.Proposal) bool {
		target := p.Time.UnixMilli()
		lo, hi, idx := 0, len(pts)-1, -1
		for lo <= hi {
			mid := (lo + hi) / 2
			if pts[mid].t <= target {
				idx = mid
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		if idx < 0 {
			return false // before MA warmup → treat as risk-off
		}
		return pts[idx].on
	}
}

// resolveRange returns [start, end); -start/-end (YYYY-MM-DD) override the
// days-back default.
func resolveRange(startStr, endStr string, days int) (time.Time, time.Time) {
	end := time.Now().UTC()
	if endStr != "" {
		if t, err := time.Parse("2006-01-02", endStr); err == nil {
			end = t
		} else {
			fail("bad -end %q: %v", endStr, err)
		}
	}
	start := end.AddDate(0, 0, -days)
	if startStr != "" {
		if t, err := time.Parse("2006-01-02", startStr); err == nil {
			start = t
		} else {
			fail("bad -start %q: %v", startStr, err)
		}
	}
	return start, end
}

func loadData(top int, start, end time.Time, dataDir, interval string, refresh bool) map[string]series {
	client := binance.NewClient()
	fmt.Printf("selecting top-%d USDT universe...\n", top)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: top})
	cancel()
	if err != nil {
		fail("universe: %v", err)
	}
	out := make(map[string]series)
	for _, s := range syms {
		ks, err := dataset.LoadKlines(context.Background(), client, dataDir, s.Symbol, interval, start, end, refresh)
		if err != nil || len(ks) < 300 {
			continue
		}
		out[s.Symbol] = series{tick: s.TickSize, ks: ks}
	}
	if len(out) == 0 {
		fail("no usable symbols")
	}
	return out
}

func bounds(all map[string]series) (minT, maxT time.Time) {
	for _, s := range all {
		if len(s.ks) == 0 {
			continue
		}
		f, l := s.ks[0].OpenTime, s.ks[len(s.ks)-1].OpenTime
		if minT.IsZero() || f.Before(minT) {
			minT = f
		}
		if maxT.IsZero() || l.After(maxT) {
			maxT = l
		}
	}
	return
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
