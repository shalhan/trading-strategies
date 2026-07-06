// Command backtest replays historical Binance 5m klines for one symbol through
// the failed-break strategy and reports performance. Per CLAUDE.md's build
// order, this single-symbol path exists to debug the strategy on one trade log
// before running the full universe.
//
// Klines are cached on disk (data/klines/SYMBOL-INTERVAL.ndjson); a second run
// over the same range reuses the cache and needs no network. Use -refresh to
// re-fetch. Every parameter is a flag and every trade is logged (out/).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/backtest"
	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

func main() {
	fs := flag.NewFlagSet("backtest", flag.ExitOnError)
	var (
		symbol   = fs.String("symbol", "ETHFIUSDT", "trading pair")
		interval = fs.String("interval", "5m", "kline interval")
		days     = fs.Int("days", 60, "lookback window in days from now")
		dataDir  = fs.String("datadir", "./data", "cache directory for klines")
		outDir   = fs.String("out", "./out", "directory for the trade-log CSV")
		refresh  = fs.Bool("refresh", false, "force re-fetch even if cached")

		atrPeriod   = fs.Int("atr-period", 14, "Wilder ATR period for the stop filter")
		// Empirically on ETHFI the failed-break stop distance is ~2–19× ATR
		// (median ~4), so a starting filter below ~2 takes nothing. 3.0 is a
		// usable starting point — TUNE per symbol on the backtest.
		maxStopATR  = fs.Float64("max-stop-atr", 3.0, "skip setups with stop distance > this × ATR (TUNE)")
		maxAttempts = fs.Int("max-attempts", 2, "max entries per side per NY day")
		bufferTicks = fs.Float64("stop-buffer-ticks", 2, "stop buffer beyond the extreme, in ticks")
		tickSize    = fs.Float64("tick-size", 0.0001, "price tick size for the symbol")

		capital = fs.Float64("capital", 10000, "starting capital for the account simulation")
		risk    = fs.Float64("risk", 0.01, "risk per trade as a fraction of capital")
	)
	_ = fs.Parse(os.Args[1:])

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		fail("load America/New_York location: %v", err)
	}

	end := time.Now().UTC()
	start := end.AddDate(0, 0, -*days)

	ks, err := loadKlines(*dataDir, *symbol, *interval, start, end, *refresh)
	if err != nil {
		fail("load klines: %v", err)
	}
	if len(ks) == 0 {
		fail("no klines for %s in the requested window", *symbol)
	}

	cfg := strategy.Config{
		Symbol: *symbol, Loc: loc,
		ATRPeriod: *atrPeriod, MaxStopATR: *maxStopATR,
		MaxAttemptsPerSide: *maxAttempts,
		StopBufferTicks:    *bufferTicks, TickSize: *tickSize,
	}
	trades := backtest.RunSymbol(cfg, ks)
	stats := backtest.Compute(trades, *capital, *risk)

	logPath := fmt.Sprintf("%s/%s-%s-trades.csv", *outDir, *symbol, *interval)
	if err := backtest.WriteTradeLogCSV(logPath, trades); err != nil {
		fail("write trade log: %v", err)
	}

	printReport(*symbol, ks, stats, *maxStopATR, logPath)
}

// loadKlines returns cached klines if present (and not refreshing), else fetches
// from Binance and caches them.
func loadKlines(dataDir, symbol, interval string, start, end time.Time, refresh bool) ([]kline.Kline, error) {
	cache := binance.CachePath(dataDir, symbol, interval)
	if !refresh {
		if ks, err := binance.LoadKlines(cache); err == nil && len(ks) > 0 {
			fmt.Printf("loaded %d cached klines from %s\n", len(ks), cache)
			return ks, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}

	fmt.Printf("fetching %s %s klines from Binance (%s → %s)...\n",
		symbol, interval, start.Format("2006-01-02"), end.Format("2006-01-02"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ks, err := binance.NewClient().Klines(ctx, symbol, interval, start, end)
	if err != nil {
		return nil, err
	}
	if err := binance.SaveKlines(cache, ks); err != nil {
		return nil, fmt.Errorf("cache klines: %w", err)
	}
	fmt.Printf("fetched and cached %d klines to %s\n", len(ks), cache)
	return ks, nil
}

func printReport(symbol string, ks []kline.Kline, s backtest.Stats, maxStopATR float64, logPath string) {
	first, last := ks[0].OpenTime, ks[len(ks)-1].OpenTime
	fmt.Printf("\n=== Backtest: %s ===\n", symbol)
	fmt.Printf("Klines:        %d  (%s → %s)\n", len(ks),
		first.Format("2006-01-02"), last.Format("2006-01-02"))
	fmt.Printf("MAX_STOP_ATR:  %.2f\n\n", maxStopATR)

	fmt.Printf("Trades:        %d  (target %d / stop %d / EOD %d)\n",
		s.Trades, s.Targets, s.Stops, s.EODs)
	fmt.Printf("Win rate:      %.1f%%  (%d W / %d L)\n", s.WinRate*100, s.Wins, s.Losses)
	fmt.Printf("Total R:       %+.2fR\n", s.TotalR)
	fmt.Printf("Expectancy:    %+.3fR / trade\n", s.ExpectancyR)
	fmt.Printf("Profit factor: %.2f\n", s.ProfitFactor)
	fmt.Printf("Max drawdown:  %.2fR\n", s.MaxDrawdownR)
	if s.StartCapital > 0 {
		ret := (s.EndEquity - s.StartCapital) / s.StartCapital * 100
		fmt.Printf("Account:       $%.2f → $%.2f  (%+.1f%%, risk %.1f%%/trade)\n",
			s.StartCapital, s.EndEquity, ret, s.RiskPerTrade*100)
	}
	fmt.Printf("\nTrade log:     %s\n", logPath)
	if s.Trades == 0 {
		fmt.Println("\nNo trades. Widen the date range or check MAX_STOP_ATR / tick-size.")
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
