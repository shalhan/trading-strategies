// Command scan runs the multi-symbol portfolio backtest: it selects a
// liquidity-filtered universe of Binance USDT pairs, replays each on a shared
// 5m timeline through its own failed-break engine, and applies the portfolio
// risk layer (concurrent + total-risk caps, rank-and-take-best) across them.
//
// Klines are cached per symbol under data/klines/; reruns reuse the cache.
// Every parameter is a flag and every trade is logged (out/).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/backtest"
	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/dataset"
	"github.com/shalhan/orderflow-trading-app/internal/portfolio"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
	"github.com/shalhan/orderflow-trading-app/internal/structure"
	"github.com/shalhan/orderflow-trading-app/internal/universe"
)

func main() {
	top := flag.Int("top", 20, "universe size: top-N USDT pairs by 24h volume")
	days := flag.Int("days", 30, "lookback window in days")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	outDir := flag.String("out", "./out", "trade-log output directory")
	interval := flag.String("interval", "5m", "kline interval")
	refresh := flag.Bool("refresh", false, "force re-fetch klines and universe")

	strat := flag.String("strategy", "failbreak", "engine: failbreak | structure")
	pivotN := flag.Int("pivot-n", 3, "structure: swing strength (bars each side)")
	targetR := flag.Float64("target-r", 2, "structure: fixed R target when not trailing")
	signals := flag.String("signals", "both", "structure: which breaks to trade: both | bos | choch")
	longOnly := flag.Bool("long-only", false, "structure: spot mode, long entries only")
	useFVG := flag.Bool("fvg", false, "structure: enter via limit in the Fair Value Gap (maker)")
	breakEven := flag.Bool("break-even", false, "structure: move stop to BEP once price reaches the prev high/low")
	fvgMid := flag.Bool("fvg-mid", false, "structure: enter at the FVG midpoint instead of its edge")
	moveStopOnBOS := flag.Bool("move-stop-on-bos", false, "structure: trail stop to latest swing on each new BOS")
	scaleOut := flag.Bool("scale-out", false, "structure: laddered exit — take scale-fraction of the runner at each scale-step-r milestone, trailing the stop one step behind (overrides fixed target/trail)")
	scaleStepR := flag.Float64("scale-step-r", 2, "structure: R spacing between scale-outs (2 = take at 2R,4R,6R…)")
	scaleFraction := flag.Float64("scale-fraction", 0.5, "structure: fraction of the remaining position taken at each scale-out")
	requireSweep := flag.Bool("require-sweep", false, "structure: only enter a break that followed a liquidity sweep of the opposite side within -sweep-lookback bars")
	sweepLookback := flag.Int("sweep-lookback", 10, "structure: how many bars back a qualifying sweep may be")
	htfAlign := flag.Bool("htf-align", false, "structure: loose HTF filter — skip entries taken directly against the higher-timeframe trend")
	htfInterval := flag.String("htf-interval", "1d", "structure: higher timeframe for -htf-align (e.g. 1d, 12h)")
	htfPivotN := flag.Int("htf-pivot-n", 3, "structure: swing strength for the HTF trend")
	liqSweep := flag.Bool("liq-sweep", false, "structure: liquidity-sweep entry (wick+reject a swing, then high-impact FVG)")
	fvgMinATR := flag.Float64("fvg-min-atr", 0, "structure: min FVG size in ATR units (high-impact filter)")
	makerFee := flag.Float64("maker-fee", 0.0002, "maker fee per side for FVG limit entries")
	minKlinesFlag := flag.Int("min-klines", 300, "skip symbols with fewer than this many candles")
	minStopPct := flag.Float64("min-stop-pct", 0, "structure: skip trades whose stop is tighter than this %% of price")
	atrPeriod := flag.Int("atr-period", 14, "Wilder ATR period")
	maxStopATR := flag.Float64("max-stop-atr", 3.0, "skip setups with stop distance > this × ATR (TUNE)")
	maxAttempts := flag.Int("max-attempts", 2, "max entries per side per NY day")
	bufferTicks := flag.Float64("stop-buffer-ticks", 2, "stop buffer beyond the extreme, in ticks")
	holdOvernight := flag.Bool("hold-overnight", false, "let positions run past NY midnight to stop/target (no force-close)")
	trailATR := flag.Float64("trail-atr", 0, "replace the 2R target with an ATR trailing stop of this many ATR (0 = fixed 2R)")
	trendFast := flag.Int("trend-fast", 0, "fast EMA period for the trend filter (0 disables)")
	trendSlow := flag.Int("trend-slow", 0, "slow EMA period for the trend filter (0 disables)")
	trendThreshold := flag.Float64("trend-threshold", 1.0, "skip fades against a trend stronger than this (in ATR units)")
	trendSkipWith := flag.Bool("trend-skip-with", false, "invert filter: skip with-trend fades, keep counter-trend ones")

	capital := flag.Float64("capital", 10000, "starting capital")
	risk := flag.Float64("risk", 0.01, "risk per trade as a fraction of capital")
	feeRate := flag.Float64("fee-rate", 0.0005, "taker fee per side, fraction of notional (0.0005 = 0.05%)")
	slipRate := flag.Float64("slip-rate", 0.0002, "slippage per side, fraction of price (0.0002 = 0.02%)")
	maxConc := flag.Int("max-concurrent", 5, "max simultaneous open positions")
	maxTotalRisk := flag.Float64("max-total-risk", 0.05, "max total open risk as a fraction of capital")
	flag.Parse()

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		fail("load America/New_York: %v", err)
	}
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)
	client := binance.NewClient()

	// 1. Select the universe.
	fmt.Printf("selecting top-%d USDT universe by 24h volume...\n", *top)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: *top})
	cancel()
	if err != nil {
		fail("select universe: %v", err)
	}
	fmt.Printf("universe: %d symbols (%s … %s)\n", len(syms), symName(syms, 0), symName(syms, len(syms)-1))

	// 2. Load klines and build one engine per symbol.
	data := make(map[string]backtest.SymbolData, len(syms))
	minKlines := *minKlinesFlag // insufficient-history filter
	for i, s := range syms {
		ks, err := dataset.LoadKlines(context.Background(), client, *dataDir, s.Symbol, *interval, start, end, *refresh)
		if err != nil {
			fmt.Printf("  [%d/%d] %-14s skip: %v\n", i+1, len(syms), s.Symbol, err)
			continue
		}
		if len(ks) < minKlines {
			fmt.Printf("  [%d/%d] %-14s skip: insufficient history (%d candles)\n", i+1, len(syms), s.Symbol, len(ks))
			continue
		}
		var eng backtest.Engine
		if *strat == "structure" {
			eng = structure.New(structure.Config{
				Symbol: s.Symbol, PivotN: *pivotN, ATRPeriod: *atrPeriod,
				MaxStopATR: *maxStopATR, MinStopFrac: *minStopPct / 100, TrailATR: *trailATR, TargetR: *targetR,
				StopBufferTicks: *bufferTicks, TickSize: s.TickSize, Signals: *signals,
				LongOnly: *longOnly, UseFVG: *useFVG, BreakEven: *breakEven,
				FVGMidpoint: *fvgMid, MoveStopOnBOS: *moveStopOnBOS,
				LiquiditySweep: *liqSweep, FVGMinATR: *fvgMinATR,
				ScaleOut: *scaleOut, ScaleStepR: *scaleStepR, ScaleFraction: *scaleFraction,
				RequireSweep: *requireSweep, SweepLookback: *sweepLookback,
				HTFAlign: *htfAlign, HTFPeriod: intervalDuration(*htfInterval), HTFPivotN: *htfPivotN,
			})
		} else {
			eng = strategy.New(strategy.Config{
				Symbol: s.Symbol, Loc: loc,
				ATRPeriod: *atrPeriod, MaxStopATR: *maxStopATR,
				MaxAttemptsPerSide: *maxAttempts,
				StopBufferTicks:    *bufferTicks, TickSize: s.TickSize,
				HoldOvernight:  *holdOvernight,
				TrendFastEMA:   *trendFast,
				TrendSlowEMA:   *trendSlow,
				TrendThreshold: *trendThreshold,
				TrendSkipWith:  *trendSkipWith,
				TrailATR:       *trailATR,
			})
		}
		data[s.Symbol] = backtest.SymbolData{Engine: eng, Klines: ks}
		fmt.Printf("  [%d/%d] %-14s %d candles\n", i+1, len(syms), s.Symbol, len(ks))
	}
	if len(data) == 0 {
		fail("no symbols with sufficient data")
	}

	// 3. Run the portfolio backtest.
	mgr := portfolio.New(portfolio.Config{
		Capital: *capital, RiskPerTrade: *risk,
		MaxConcurrent: *maxConc, MaxTotalRisk: *maxTotalRisk,
	})
	trades := backtest.RunPortfolio(data, mgr)
	costs := backtest.Costs{FeeRate: *feeRate, SlipRate: *slipRate, MakerFee: *makerFee}
	netTrades := costs.Apply(trades)
	gross := backtest.Compute(trades, *capital, *risk)
	net := backtest.Compute(netTrades, *capital, *risk)

	logPath := fmt.Sprintf("%s/portfolio-%s-trades.csv", *outDir, *interval)
	if err := backtest.WriteTradeLogCSV(logPath, trades); err != nil {
		fail("write trade log: %v", err)
	}

	report(len(data), *days, *maxStopATR, *maxConc, *maxTotalRisk, gross, net, costs.AvgPerTradeR(trades), netTrades, logPath)
}

func report(nSym, days int, maxStopATR float64, maxConc int, maxTotalRisk float64, g, n backtest.Stats, avgCostR float64, netTrades []*strategy.Trade, logPath string) {
	fmt.Printf("\n=== Portfolio backtest ===\n")
	fmt.Printf("Symbols:       %d   Window: %d days\n", nSym, days)
	fmt.Printf("Caps:          MAX_STOP_ATR %.2f | concurrent %d | total risk %.0f%%\n",
		maxStopATR, maxConc, maxTotalRisk*100)
	fmt.Printf("\nTrades:        %d  (target %d / stop %d / trail %d / EOD %d)\n", g.Trades, g.Targets, g.Stops, g.Trails, g.EODs)
	fmt.Printf("Win rate:      %.1f%%  (%d W / %d L)\n", g.WinRate*100, g.Wins, g.Losses)

	fmt.Printf("\n%-14s %12s %12s\n", "", "GROSS", "NET (after costs)")
	fmt.Printf("%-14s %+11.2fR %+11.2fR\n", "Total R", g.TotalR, n.TotalR)
	fmt.Printf("%-14s %+11.3fR %+11.3fR\n", "Expectancy", g.ExpectancyR, n.ExpectancyR)
	fmt.Printf("%-14s %12.2f %12.2f\n", "Profit factor", g.ProfitFactor, n.ProfitFactor)
	fmt.Printf("%-14s %11.2fR %11.2fR\n", "Max drawdown", g.MaxDrawdownR, n.MaxDrawdownR)
	fmt.Printf("Avg cost/trade: %.3fR\n", avgCostR)
	if g.StartCapital > 0 {
		gret := (g.EndEquity - g.StartCapital) / g.StartCapital * 100
		nret := (n.EndEquity - n.StartCapital) / n.StartCapital * 100
		fmt.Printf("Account:        gross $%.0f (%+.1f%%)   net $%.0f (%+.1f%%)\n",
			g.EndEquity, gret, n.EndEquity, nret)
	}

	// Setup breakdown (BOS vs CHoCH) when tagged, on net R.
	setupR := map[string]float64{}
	setupN := map[string]int{}
	for _, t := range netTrades {
		if t.Setup != "" {
			setupR[t.Setup] += t.R
			setupN[t.Setup]++
		}
	}
	if len(setupR) > 0 {
		fmt.Printf("\nBy setup (net R):\n")
		for _, k := range []string{"BOS", "CHoCH"} {
			if c := setupN[k]; c > 0 {
				fmt.Printf("  %-6s %+7.2fR  (%d trades, %+.3fR/trade)\n", k, setupR[k], c, setupR[k]/float64(c))
			}
		}
	}

	// Full per-symbol P&L (net), sorted best to worst.
	type row struct {
		sym     string
		r       float64
		n, wins int
	}
	agg := map[string]*row{}
	for _, t := range netTrades {
		a := agg[t.Symbol]
		if a == nil {
			a = &row{sym: t.Symbol}
			agg[t.Symbol] = a
		}
		a.r += t.R
		a.n++
		if t.R > 0 {
			a.wins++
		}
	}
	rows := make([]*row, 0, len(agg))
	for _, a := range agg {
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].r > rows[j].r })
	if len(rows) > 0 {
		fmt.Printf("\nPer-symbol P&L (net R):\n")
		fmt.Printf("  %-14s %8s %8s %7s\n", "symbol", "netR", "trades", "win%")
		var winners, losers int
		for _, a := range rows {
			wr := 0.0
			if a.n > 0 {
				wr = float64(a.wins) / float64(a.n) * 100
			}
			fmt.Printf("  %-14s %+8.2f %8d %6.0f%%\n", a.sym, a.r, a.n, wr)
			if a.r > 0 {
				winners++
			} else if a.r < 0 {
				losers++
			}
		}
		fmt.Printf("  %d symbols net-positive, %d net-negative\n", winners, losers)
	}
	fmt.Printf("\nTrade log:     %s\n", logPath)
}

// intervalDuration parses a Binance interval ("15m","4h","1d") to a Duration,
// defaulting to 24h on anything unrecognized. Used for the HTF-align bar length.
func intervalDuration(iv string) time.Duration {
	if len(iv) < 2 {
		return 24 * time.Hour
	}
	n, err := strconv.Atoi(iv[:len(iv)-1])
	if err != nil || n <= 0 {
		return 24 * time.Hour
	}
	switch iv[len(iv)-1] {
	case 'm':
		return time.Duration(n) * time.Minute
	case 'h':
		return time.Duration(n) * time.Hour
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

func symName(s []universe.Symbol, i int) string {
	if i < 0 || i >= len(s) {
		return "-"
	}
	return s[i].Symbol
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
