// Command tune sweeps strategy/portfolio parameters over a grid and reports
// each combination's performance on an in-sample (train) split and a held-out
// out-of-sample (test) split. The split is the whole point: a parameter set
// that looks great in-sample but falls apart out-of-sample is curve-fit, not an
// edge. There are no universal values (CLAUDE.md) — pick a combo that holds up
// on BOTH splits.
//
// Data (universe + klines) is loaded once and cached; the grid then runs purely
// in memory.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

type combo struct {
	stopATR    float64
	attempts   int
	concurrent int
}

type result struct {
	combo
	train backtest.Stats
	test  backtest.Stats
}

func main() {
	top := flag.Int("top", 20, "universe size: top-N USDT pairs by 24h volume")
	days := flag.Int("days", 45, "lookback window in days")
	trainFrac := flag.Float64("train", 0.7, "fraction of the window used for in-sample tuning")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	outDir := flag.String("out", "./out", "results output directory")
	interval := flag.String("interval", "5m", "kline interval")
	refresh := flag.Bool("refresh", false, "force re-fetch")

	stopGrid := flag.String("stop-atr", "2,2.5,3,4,5", "MAX_STOP_ATR values to sweep")
	attemptGrid := flag.String("attempts", "1,2,3", "MAX_ATTEMPTS_PER_SIDE values to sweep")
	concGrid := flag.String("concurrent", "3,5,8", "MAX_CONCURRENT_POSITIONS values to sweep")
	objective := flag.String("objective", "totalr", "ranking metric: totalr | expectancy | pf")
	show := flag.Int("show", 15, "how many top combos to print")

	atrPeriod := flag.Int("atr-period", 14, "Wilder ATR period (fixed)")
	bufferTicks := flag.Float64("stop-buffer-ticks", 2, "stop buffer in ticks (fixed)")
	trailATR := flag.Float64("trail-atr", 0, "ATR trailing stop instead of 2R target (0 = fixed 2R, fixed)")
	holdOvernight := flag.Bool("hold-overnight", false, "no NY-midnight force-close (fixed)")
	stratType := flag.String("strategy", "failbreak", "engine: failbreak | structure")
	pivotN := flag.Int("pivot-n", 3, "structure: swing strength (fixed)")
	targetR := flag.Float64("target-r", 2, "structure: fixed R target when not trailing (fixed)")
	signals := flag.String("signals", "both", "structure: both | bos | choch (fixed)")
	longOnly := flag.Bool("long-only", false, "structure: spot mode, long only (fixed)")
	capital := flag.Float64("capital", 10000, "starting capital (fixed)")
	risk := flag.Float64("risk", 0.01, "risk per trade (fixed)")
	feeRate := flag.Float64("fee-rate", 0.0005, "taker fee per side (0.0005 = 0.05%)")
	slipRate := flag.Float64("slip-rate", 0.0002, "slippage per side (0.0002 = 0.02%)")
	flag.Parse()
	costs := backtest.Costs{FeeRate: *feeRate, SlipRate: *slipRate}

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		fail("load America/New_York: %v", err)
	}
	stopVals := parseFloats(*stopGrid)
	attemptVals := parseInts(*attemptGrid)
	concVals := parseInts(*concGrid)
	if len(stopVals) == 0 || len(attemptVals) == 0 || len(concVals) == 0 {
		fail("empty parameter grid")
	}

	ep := engineParams{
		strat: *stratType, atrPeriod: *atrPeriod, bufferTicks: *bufferTicks,
		capital: *capital, risk: *risk, trailATR: *trailATR,
		holdOvernight: *holdOvernight, pivotN: *pivotN, targetR: *targetR,
		signals: *signals, longOnly: *longOnly,
	}

	all := loadData(*top, *days, *dataDir, *interval, *refresh)
	train, test, cutoff := split(all, *trainFrac)
	fmt.Printf("loaded %d symbols; split at %s (train %.0f%% / test %.0f%%)\n",
		len(all), cutoff.Format("2006-01-02"), *trainFrac*100, (1-*trainFrac)*100)
	fmt.Printf("sweeping %d combos (stop-atr × attempts × concurrent = %d × %d × %d)...\n\n",
		len(stopVals)*len(attemptVals)*len(concVals), len(stopVals), len(attemptVals), len(concVals))

	var results []result
	for _, sa := range stopVals {
		for _, at := range attemptVals {
			for _, cc := range concVals {
				c := combo{stopATR: sa, attempts: at, concurrent: cc}
				results = append(results, result{
					combo: c,
					train: runCombo(train, loc, c, ep, costs),
					test:  runCombo(test, loc, c, ep, costs),
				})
			}
		}
	}

	sort.SliceStable(results, func(i, j int) bool {
		return objVal(results[i].train, *objective) > objVal(results[j].train, *objective)
	})

	printTable(results, *objective, *show)
	csvPath := filepath.Join(*outDir, "tune-results.csv")
	if err := writeCSV(csvPath, results); err != nil {
		fail("write results: %v", err)
	}
	fmt.Printf("\nFull grid:     %s\n", csvPath)
	fmt.Println("\nPick a combo that is positive and stable on BOTH train and test.")
	fmt.Println("A combo that is strong on train but weak/negative on test is overfit.")
}

// runCombo builds fresh engines for the given strategy params and runs the
// portfolio backtest. Total risk is set so the concurrent cap is the binding
// one (= concurrent × risk).
// engineParams holds the per-run engine settings fixed across the sweep.
type engineParams struct {
	strat         string
	atrPeriod     int
	bufferTicks   float64
	capital       float64
	risk          float64
	trailATR      float64
	holdOvernight bool
	pivotN        int
	targetR       float64
	signals       string
	longOnly      bool
}

func runCombo(data map[string]series, loc *time.Location, c combo, ep engineParams, costs backtest.Costs) backtest.Stats {
	sd := make(map[string]backtest.SymbolData, len(data))
	for sym, s := range data {
		var eng backtest.Engine
		if ep.strat == "structure" {
			eng = structure.New(structure.Config{
				Symbol: sym, PivotN: ep.pivotN, ATRPeriod: ep.atrPeriod,
				MaxStopATR: c.stopATR, TrailATR: ep.trailATR, TargetR: ep.targetR,
				StopBufferTicks: ep.bufferTicks, TickSize: s.tick, Signals: ep.signals,
				LongOnly: ep.longOnly,
			})
		} else {
			eng = strategy.New(strategy.Config{
				Symbol: sym, Loc: loc, ATRPeriod: ep.atrPeriod, MaxStopATR: c.stopATR,
				MaxAttemptsPerSide: c.attempts, StopBufferTicks: ep.bufferTicks, TickSize: s.tick,
				TrailATR: ep.trailATR, HoldOvernight: ep.holdOvernight,
			})
		}
		sd[sym] = backtest.SymbolData{Engine: eng, Klines: s.ks}
	}
	mgr := portfolio.New(portfolio.Config{
		Capital: ep.capital, RiskPerTrade: ep.risk,
		MaxConcurrent: c.concurrent, MaxTotalRisk: float64(c.concurrent) * ep.risk,
	})
	// Rank on NET performance — gross numbers are meaningless once costs apply.
	return backtest.Compute(costs.Apply(backtest.RunPortfolio(sd, mgr)), ep.capital, ep.risk)
}

func loadData(top, days int, dataDir, interval string, refresh bool) map[string]series {
	client := binance.NewClient()
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -days)

	fmt.Printf("selecting top-%d USDT universe...\n", top)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: top})
	cancel()
	if err != nil {
		fail("select universe: %v", err)
	}

	out := make(map[string]series)
	const minKlines = 300
	for i, s := range syms {
		ks, err := dataset.LoadKlines(context.Background(), client, dataDir, s.Symbol, interval, start, end, refresh)
		if err != nil || len(ks) < minKlines {
			fmt.Printf("  [%d/%d] %-14s skip\n", i+1, len(syms), s.Symbol)
			continue
		}
		out[s.Symbol] = series{tick: s.TickSize, ks: ks}
		fmt.Printf("  [%d/%d] %-14s %d candles\n", i+1, len(syms), s.Symbol, len(ks))
	}
	if len(out) == 0 {
		fail("no symbols with sufficient data")
	}
	return out
}

// split partitions each symbol's series at a single absolute cutoff time so
// train and test cover the same calendar windows across all symbols. The test
// engines run fresh (ATR re-warms; the first partial NY day is skipped by the
// incomplete-window guard) — so there is no lookahead from train into test.
func split(all map[string]series, frac float64) (train, test map[string]series, cutoff time.Time) {
	var minT, maxT time.Time
	for _, s := range all {
		if len(s.ks) == 0 {
			continue
		}
		first, last := s.ks[0].OpenTime, s.ks[len(s.ks)-1].OpenTime
		if minT.IsZero() || first.Before(minT) {
			minT = first
		}
		if maxT.IsZero() || last.After(maxT) {
			maxT = last
		}
	}
	span := maxT.Sub(minT)
	cutoff = minT.Add(time.Duration(float64(span) * frac))

	train = make(map[string]series, len(all))
	test = make(map[string]series, len(all))
	for sym, s := range all {
		var tr, te []kline.Kline
		for _, k := range s.ks {
			if k.OpenTime.Before(cutoff) {
				tr = append(tr, k)
			} else {
				te = append(te, k)
			}
		}
		train[sym] = series{tick: s.tick, ks: tr}
		test[sym] = series{tick: s.tick, ks: te}
	}
	return train, test, cutoff
}

func printTable(results []result, objective string, show int) {
	fmt.Printf("%-9s %-4s %-5s │ %-28s │ %-22s\n", "stopATR", "att", "conc", "TRAIN (in-sample)", "TEST (out-of-sample)")
	fmt.Printf("%-9s %-4s %-5s │ %5s %7s %6s %5s │ %5s %7s %6s\n",
		"", "", "", "trades", "totR", "exp", "PF", "trades", "totR", "PF")
	fmt.Println(strings.Repeat("─", 78))
	for i, r := range results {
		if i >= show {
			break
		}
		fmt.Printf("%-9.2f %-4d %-5d │ %5d %+7.2f %+6.3f %5.2f │ %5d %+7.2f %6.2f\n",
			r.stopATR, r.attempts, r.concurrent,
			r.train.Trades, r.train.TotalR, r.train.ExpectancyR, r.train.ProfitFactor,
			r.test.Trades, r.test.TotalR, r.test.ProfitFactor)
	}
	fmt.Printf("\n(ranked by train %s)\n", objective)
}

func writeCSV(path string, results []result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{
		"stop_atr", "attempts", "concurrent",
		"train_trades", "train_totalR", "train_exp", "train_pf", "train_dd",
		"test_trades", "test_totalR", "test_exp", "test_pf", "test_dd",
	})
	for _, r := range results {
		w.Write([]string{
			ftoa(r.stopATR), itoa(r.attempts), itoa(r.concurrent),
			itoa(r.train.Trades), ftoa(r.train.TotalR), ftoa(r.train.ExpectancyR), ftoa(r.train.ProfitFactor), ftoa(r.train.MaxDrawdownR),
			itoa(r.test.Trades), ftoa(r.test.TotalR), ftoa(r.test.ExpectancyR), ftoa(r.test.ProfitFactor), ftoa(r.test.MaxDrawdownR),
		})
	}
	return w.Error()
}

func objVal(s backtest.Stats, objective string) float64 {
	switch objective {
	case "expectancy":
		return s.ExpectancyR
	case "pf":
		return s.ProfitFactor
	default:
		return s.TotalR
	}
}

func parseFloats(s string) []float64 {
	var out []float64
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if v, err := strconv.ParseFloat(p, 64); err == nil {
				out = append(out, v)
			}
		}
	}
	return out
}

func parseInts(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			if v, err := strconv.Atoi(p); err == nil {
				out = append(out, v)
			}
		}
	}
	return out
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }
func itoa(i int) string     { return strconv.Itoa(i) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
