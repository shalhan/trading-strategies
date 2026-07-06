// Command vwapscan is a live VWAP-proximity screener: for each symbol in a
// liquidity-filtered universe it fetches recent klines, computes the session
// VWAP (anchored to the New York day, like the rest of the bot), and reports the
// symbols whose latest price sits CLOSE TO — "almost touching" — the VWAP line.
//
// "Close" is measured in ATR units by default (-max-dist-atr), so the threshold
// behaves consistently across coins of different volatility, matching the
// project's preference for ATR-based filters over fixed percentages. Pass
// -max-dist-pct to switch to a percentage-of-price threshold instead.
//
// It answers "which coins are pulling back into VWAP right now?" — a current
// snapshot, computed on closed candles only (the still-open final candle is
// excluded). Use -watch to re-scan at each candle close and stream the feed.
//
// This is a screen, not a strategy: it computes no P&L and opens no positions.
// VWAP reversion / rejection is a setup idea to eyeball, not a backtested edge.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/dataset"
	"github.com/shalhan/orderflow-trading-app/internal/indicator"
	"github.com/shalhan/orderflow-trading-app/internal/universe"
)

// hit is one symbol whose latest price is within the proximity threshold of VWAP.
type hit struct {
	symbol  string
	last    float64
	vwap    float64
	atr     float64
	distAbs float64 // |last - vwap|, price units
	distATR float64 // |last - vwap| / ATR
	distPct float64 // |last - vwap| / last * 100
	above   bool    // price is above VWAP (else below)
}

type scanCfg struct {
	dataDir, interval string
	days, minKlines    int
	atrPeriod          int
	maxDistATR         float64 // proximity threshold in ATR units; <=0 disables ATR mode
	maxDistPct         float64 // proximity threshold in % of price; >0 enables percent mode
	refresh            bool
}

func main() {
	top := flag.Int("top", 100, "universe size: top-N USDT pairs by 24h volume")
	interval := flag.String("interval", "5m", "kline interval to screen")
	days := flag.Int("days", 1, "lookback window in days (must cover the current NY session)")
	atrPeriod := flag.Int("atr-period", 14, "ATR period used to scale the proximity threshold")
	maxDistATR := flag.Float64("max-dist-atr", 0.3, "report a symbol whose price is within this many ATRs of VWAP")
	maxDistPct := flag.Float64("max-dist-pct", 0, "use a percent-of-price threshold instead of ATR (e.g. 0.2 = within 0.2%); overrides -max-dist-atr when > 0")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	refresh := flag.Bool("refresh", true, "force re-fetch klines so data is current (set false to reuse cache)")
	minKlines := flag.Int("min-klines", 20, "skip symbols with fewer than this many candles this session")
	watch := flag.Bool("watch", false, "stream continuously: re-scan at each candle close and print the proximity feed")
	flag.Parse()

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		fail("load America/New_York: %v", err)
	}

	cfg := scanCfg{
		dataDir: *dataDir, interval: *interval,
		days: *days, minKlines: *minKlines, atrPeriod: *atrPeriod,
		maxDistATR: *maxDistATR, maxDistPct: *maxDistPct, refresh: *refresh,
	}
	client := binance.NewClient()

	fmt.Printf("selecting top-%d USDT universe by 24h volume...\n", *top)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: *top})
	cancel()
	if err != nil {
		fail("select universe: %v", err)
	}

	if *watch {
		watchLoop(client, syms, cfg, loc)
		return
	}

	fmt.Printf("scanning %d symbols on %s (%s, %s)...\n\n", len(syms), cfg.interval, cfg.thresholdLabel(), "session VWAP anchored to NY day")
	hits, scanned := scanOnce(client, syms, cfg, loc)
	report(cfg, scanned, len(syms), hits)
}

// thresholdLabel renders the active proximity rule for headers.
func (c scanCfg) thresholdLabel() string {
	if c.maxDistPct > 0 {
		return fmt.Sprintf("within %.3g%% of VWAP", c.maxDistPct)
	}
	return fmt.Sprintf("within %.3g ATR of VWAP", c.maxDistATR)
}

// within reports whether a candidate's distance to VWAP passes the active
// threshold (percent mode if -max-dist-pct > 0, else ATR mode).
func (c scanCfg) within(h hit) bool {
	if c.maxDistPct > 0 {
		return h.distPct <= c.maxDistPct
	}
	return h.atr > 0 && h.distATR <= c.maxDistATR
}

// closeness returns the metric used to rank hits (tightest first): % in percent
// mode, ATR units otherwise.
func (c scanCfg) closeness(h hit) float64 {
	if c.maxDistPct > 0 {
		return h.distPct
	}
	return h.distATR
}

// scanOnce loads fresh klines for each symbol, computes the session VWAP and ATR
// up to the last CLOSED candle, and returns the symbols whose latest price is
// within the proximity threshold, closest first. The window ends at now, so each
// call is current.
func scanOnce(client *binance.Client, syms []universe.Symbol, c scanCfg, loc *time.Location) (hits []hit, scanned int) {
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -c.days)

	for _, s := range syms {
		ks, err := dataset.LoadKlines(context.Background(), client, c.dataDir, s.Symbol, c.interval, start, end, c.refresh)
		if err != nil || len(ks) < c.minKlines {
			continue
		}
		scanned++

		vwap := indicator.NewSessionVWAP(loc)
		atr := indicator.NewATR(c.atrPeriod)
		var vw, av float64
		var vwReady bool
		for i := range ks {
			vw, vwReady = vwap.Update(ks[i])
			av, _ = atr.Update(ks[i])
		}
		if !vwReady {
			continue // no volume this session yet
		}

		last := ks[len(ks)-1].Close
		distAbs := math.Abs(last - vw)
		h := hit{
			symbol: s.Symbol, last: last, vwap: vw, atr: av,
			distAbs: distAbs,
			distPct: distAbs / last * 100,
			above:   last >= vw,
		}
		if av > 0 {
			h.distATR = distAbs / av
		}
		if !c.within(h) {
			continue
		}
		hits = append(hits, h)
	}

	sort.Slice(hits, func(i, j int) bool { return c.closeness(hits[i]) < c.closeness(hits[j]) })
	return hits, scanned
}

// watchLoop re-scans at each candle close and prints the current proximity feed.
func watchLoop(client *binance.Client, syms []universe.Symbol, c scanCfg, loc *time.Location) {
	c.refresh = true
	d := intervalDuration(c.interval)
	fmt.Printf("watching %d symbols on %s — %s, streaming each candle close (Ctrl-C to stop)\n\n",
		len(syms), c.interval, c.thresholdLabel())

	for {
		hits, _ := scanOnce(client, syms, c, loc)
		ts := time.Now().Format("15:04:05")
		if len(hits) == 0 {
			fmt.Printf("[%s] no symbols near VWAP\n", ts)
		} else {
			fmt.Printf("[%s] %d near VWAP:\n", ts, len(hits))
			for _, h := range hits {
				fmt.Printf("    %-12s %s\n", h.symbol, distLine(c, h))
			}
		}

		now := time.Now()
		next := now.Truncate(d).Add(d + 4*time.Second)
		time.Sleep(time.Until(next))
	}
}

// distLine renders the distance-to-VWAP detail for one hit.
func distLine(c scanCfg, h hit) string {
	side := "above ↑"
	if !h.above {
		side = "below ↓"
	}
	return fmt.Sprintf("last %-11s vwap %-11s  %-7s  %.3g%% / %.2f ATR",
		fmt.Sprintf("%.6g", h.last), fmt.Sprintf("%.6g", h.vwap), side, h.distPct, h.distATR)
}

// intervalDuration parses a Binance interval ("5m","1h","1d") to a Duration,
// defaulting to five minutes on anything unrecognized.
func intervalDuration(iv string) time.Duration {
	if len(iv) < 2 {
		return 5 * time.Minute
	}
	var n int
	if _, err := fmt.Sscanf(iv[:len(iv)-1], "%d", &n); err != nil || n <= 0 {
		return 5 * time.Minute
	}
	switch iv[len(iv)-1] {
	case 'm':
		return time.Duration(n) * time.Minute
	case 'h':
		return time.Duration(n) * time.Hour
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	}
	return 5 * time.Minute
}

func report(c scanCfg, scanned, total int, hits []hit) {
	fmt.Printf("=== VWAP-proximity screen (%s, %s) ===\n", c.interval, c.thresholdLabel())
	fmt.Printf("Scanned %d/%d symbols; %d near VWAP.\n\n", scanned, total, len(hits))
	if len(hits) == 0 {
		fmt.Println("No matches. Loosen with a larger -max-dist-atr (or -max-dist-pct), -top, or -days.")
		return
	}
	fmt.Printf("  %-14s %-7s %12s %12s %10s %10s\n", "symbol", "side", "last", "vwap", "dist %", "dist ATR")
	for _, h := range hits {
		side := "above"
		if !h.above {
			side = "below"
		}
		fmt.Printf("  %-14s %-7s %12.6g %12.6g %10.3g %10.2f\n",
			h.symbol, side, h.last, h.vwap, h.distPct, h.distATR)
	}
	fmt.Printf("\nside = price relative to VWAP (above = potential rejection short / below = potential reclaim long).\n")
	fmt.Printf("dist = how far the last close is from VWAP; smaller = closer to touching.\n")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
