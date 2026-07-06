// Command screen is a live structure screener (not a backtest): for each symbol
// in a liquidity-filtered universe it fetches recent klines, replays them through
// the structure engine, and reports the symbols whose MOST RECENT structure break
// is a CHoCH (Change of Character) within the last -max-age candles. It answers
// "which coins just printed a CHoCH right now?" — a current snapshot.
//
// Unlike cmd/scan it computes no P&L and opens no positions; it only reads the
// engine's classified structure events. Data is fetched up to the present minute;
// the still-open final candle is excluded, so every CHoCH is on a closed candle.
// Use -refresh to pull fresh data (the cache is reused otherwise and may be stale).
//
// With -watch it runs continuously: it re-scans at each candle close and streams
// only the newly-fired CHoCHs, so you get a live feed instead of a one-shot
// snapshot. (A CHoCH is confirmed on close, so a new signal can only appear once
// per candle no matter how often you poll; the loop wakes just after each close.)
//
// Structure is read at one of two scales (-tier). The "swing" tier (default)
// tracks the trend-defining higher-lows and lower-highs — a CHoCH fires only when
// the swing that holds the trend actually breaks, matching how a trader reads
// structure by hand. The "internal" tier tracks minor swings and flips on shallow
// pullbacks (noisy on low timeframes). -swing-n / -pivot-n set each tier's
// strength. -min-trend-bars additionally suppresses a CHoCH that merely re-flips
// a short-lived opposing leg (a re-confirmation, not a true reversal).
//
// -fvg narrows the screen to the CHoCH-entry setup: only symbols whose CHoCH left
// a fair value gap (a displacement imbalance) in the break direction, printing the
// gap zone and whether price has already retraced into it ("tapped") or it is
// still unfilled ahead ("fresh", a pending entry).
//
// -htf adds a higher-timeframe contradiction filter: a passing setup is dropped if
// an UNFILLED fair value gap of the opposite direction on the higher timeframe
// (e.g. 15m) sits in the trade's path within -htf-range — overhead supply against a
// long, support beneath a short. It keeps only 1m setups the higher timeframe does
// not actively oppose.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/dataset"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
	"github.com/shalhan/orderflow-trading-app/internal/structure"
	"github.com/shalhan/orderflow-trading-app/internal/universe"
)

// hit is one symbol whose latest reportable break is recent enough.
type hit struct {
	symbol  string
	ev      structure.StructureEvent
	barsAgo int
	last    float64 // last close, for context

	// FVG set only when -fvg is on and a fair value gap formed after the CHoCH.
	fvgLo, fvgHi float64
	fvgTapped    bool // price has already retraced into the gap (entry would have triggered)
}

// scanCfg bundles the per-scan parameters shared by the one-shot and watch paths.
type scanCfg struct {
	dataDir, interval, signal, tier                       string
	days, maxAge, pivotN, swingN, minKlines, minTrendBars int
	refresh                                               bool

	fvg       bool // require a fair value gap to form after the CHoCH (entry setup)
	fvgWindow int  // candles after the break to look for the displacement FVG

	// Higher-timeframe contradiction filter: drop a 1m setup when an unfilled HTF
	// FVG of the OPPOSITE direction lies in the trade's path (HTF flow against us).
	htf      string  // HTF interval to cross-check, e.g. "15m"; "" = off
	htfDays  int     // history to fetch for the HTF
	htfRange float64 // how close (fraction of price) an opposing HTF FVG counts as contradicting
}

// pivotForTier returns the swing strength to track for the configured tier:
// the larger swing-n for "swing" (trend-defining) structure, else the internal
// pivot-n for the minor scale.
func (c scanCfg) pivotForTier() int {
	if c.tier == "internal" {
		return c.pivotN
	}
	return c.swingN
}

func main() {
	top := flag.Int("top", 100, "universe size: top-N USDT pairs by 24h volume")
	interval := flag.String("interval", "1m", "kline interval to screen")
	days := flag.Int("days", 2, "lookback window in days (enough history to warm swings/ATR)")
	maxAge := flag.Int("max-age", 20, "only report a CHoCH within this many candles of now")
	signal := flag.String("signal", "choch", "which break to report: choch | bos | both")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	refresh := flag.Bool("refresh", true, "force re-fetch klines so data is current (set false to reuse cache)")
	tier := flag.String("tier", "swing", "structure scale: swing (trend-defining higher-lows/lower-highs) | internal (minor swings)")
	swingN := flag.Int("swing-n", 20, "swing-tier strength (bars each side); the higher-low/lower-high a CHoCH must break")
	pivotN := flag.Int("pivot-n", 5, "internal-tier strength (bars each side); used only when -tier internal")
	minTrendBars := flag.Int("min-trend-bars", 5, "reversal filter: report a CHoCH only if the trend it flips lasted at least this many candles (0 = off). Suppresses re-flips inside an ongoing trend")
	fvg := flag.Bool("fvg", false, "entry-setup filter: only report a CHoCH that left a fair value gap (displacement imbalance) in the break direction; shows the FVG zone")
	fvgWindow := flag.Int("fvg-window", 5, "candles after the CHoCH to look for the displacement FVG")
	htf := flag.String("htf", "", "higher-timeframe cross-check (e.g. 15m): drop a setup if an unfilled opposing FVG on this timeframe is in the trade's path; empty = off")
	htfDays := flag.Int("htf-days", 5, "history (days) to fetch for the higher-timeframe FVG check")
	htfRange := flag.Float64("htf-range", 0.01, "how close an opposing HTF FVG must be to count as contradicting, as a fraction of price (0.01 = 1%)")
	minKlines := flag.Int("min-klines", 100, "skip symbols with fewer than this many candles")
	watch := flag.Bool("watch", false, "stream continuously: re-scan at each candle close and print only newly-fired signals")
	flag.Parse()

	cfg := scanCfg{
		dataDir: *dataDir, interval: *interval, signal: *signal, tier: *tier,
		days: *days, maxAge: *maxAge, pivotN: *pivotN, swingN: *swingN,
		minKlines: *minKlines, minTrendBars: *minTrendBars, refresh: *refresh,
		fvg: *fvg, fvgWindow: *fvgWindow,
		htf: *htf, htfDays: *htfDays, htfRange: *htfRange,
	}
	client := binance.NewClient()

	// The universe is selected once (it changes slowly); only klines re-fetch.
	fmt.Printf("selecting top-%d USDT universe by 24h volume...\n", *top)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: *top})
	cancel()
	if err != nil {
		fail("select universe: %v", err)
	}

	if *watch {
		watchLoop(client, syms, cfg)
		return
	}

	fmt.Printf("scanning %d symbols on %s, %s tier (pivot %d, last %d days, refresh=%v)...\n\n",
		len(syms), cfg.interval, cfg.tier, cfg.pivotForTier(), cfg.days, cfg.refresh)
	hits, scanned, dropped := scanOnce(client, syms, cfg)
	report(cfg, scanned, len(syms), dropped, hits)
}

// scanOnce loads fresh klines for each symbol, replays the structure engine, and
// returns the symbols whose latest qualifying break is within max-age candles of
// now, most recent first. The window ends at time.Now(), so each call is current.
func scanOnce(client *binance.Client, syms []universe.Symbol, c scanCfg) (hits []hit, scanned, htfDropped int) {
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -c.days)
	htfStart := end.AddDate(0, 0, -c.htfDays)

	for _, s := range syms {
		ks, err := dataset.LoadKlines(context.Background(), client, c.dataDir, s.Symbol, c.interval, start, end, c.refresh)
		if err != nil || len(ks) < c.minKlines {
			continue
		}
		scanned++

		tr := structure.NewTracker(c.pivotForTier())

		// Replay; record the index of the latest reportable break. Track the
		// index of each CHoCH (trend flip) so the reversal filter can measure how
		// long the trend a CHoCH flips had lasted (BOS continues the trend, so it
		// doesn't reset the clock).
		var lastEv *structure.StructureEvent
		lastIdx := -1
		lastChochIdx := -1
		for i := range ks {
			ev := tr.Update(ks[i])
			if ev == nil {
				continue
			}

			// Reversal-quality filter: a CHoCH is only a genuine reversal if the
			// trend it flips lasted at least min-trend-bars. A quick A→B→A re-flip
			// (a shallow pullback inside an uptrend) is suppressed.
			qualifies := true
			if ev.Setup == "CHoCH" {
				prior := i // no prior CHoCH: measure from the start of the series
				if lastChochIdx >= 0 {
					prior = i - lastChochIdx
				}
				qualifies = c.minTrendBars <= 0 || prior >= c.minTrendBars
				lastChochIdx = i
			}
			if qualifies && want(c.signal, ev.Setup) {
				lastEv, lastIdx = ev, i
			}
		}
		if lastEv == nil {
			continue
		}
		barsAgo := (len(ks) - 1) - lastIdx
		if barsAgo > c.maxAge {
			continue
		}

		h := hit{symbol: s.Symbol, ev: *lastEv, barsAgo: barsAgo, last: ks[len(ks)-1].Close}
		if c.fvg {
			lo, hi, formedIdx, ok := findFVG(ks, lastIdx, lastEv.Side, c.fvgWindow)
			if !ok {
				continue // no displacement gap after the CHoCH → not the setup we want
			}
			h.fvgLo, h.fvgHi = lo, hi
			h.fvgTapped = zoneTapped(ks, formedIdx, lo, hi)
		}

		// Higher-timeframe contradiction filter: only check symbols that already
		// passed (so HTF fetches are few), and drop those whose trade fights an
		// unfilled opposing HTF FVG in its path.
		if c.htf != "" {
			htfKs, err := dataset.LoadKlines(context.Background(), client, c.dataDir, s.Symbol, c.htf, htfStart, end, c.refresh)
			if err == nil && len(htfKs) >= 3 {
				if htfContradicts(collectUnfilledFVGs(htfKs), lastEv.Side, h.last, c.htfRange) {
					htfDropped++
					continue
				}
			}
		}
		hits = append(hits, h)
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].barsAgo < hits[j].barsAgo })
	return hits, scanned, htfDropped
}

// watchLoop re-scans at each candle close and streams only newly-fired signals.
// A signal is "new" when a symbol's latest qualifying break has a later close
// time than the one we last printed for it, so a CHoCH that merely stays within
// the max-age window is reported once, not every cycle.
func watchLoop(client *binance.Client, syms []universe.Symbol, c scanCfg) {
	c.refresh = true // a live feed must always pull the freshest candles
	d := intervalDuration(c.interval)
	lastSeen := make(map[string]time.Time, len(syms))

	fmt.Printf("watching %d symbols on %s (%s tier, pivot %d) — streaming new %s as candles close (Ctrl-C to stop)\n\n",
		len(syms), c.interval, c.tier, c.pivotForTier(), c.signal)

	for {
		hits, _, _ := scanOnce(client, syms, c)
		var fresh []hit
		for _, h := range hits {
			if h.ev.Time.After(lastSeen[h.symbol]) {
				fresh = append(fresh, h)
				lastSeen[h.symbol] = h.ev.Time
			}
		}
		streamCycle(c, fresh, len(hits))

		// Wake just after the next candle close so the freshly-closed candle is
		// already available over REST.
		now := time.Now()
		next := now.Truncate(d).Add(d + 4*time.Second)
		time.Sleep(time.Until(next))
	}
}

// streamCycle prints one watch cycle: the newly-fired signals, or a heartbeat so
// the feed visibly stays alive when nothing new fired.
func streamCycle(c scanCfg, fresh []hit, active int) {
	ts := time.Now().Format("15:04:05")
	if len(fresh) == 0 {
		fmt.Printf("[%s] no new %s  (%d active in last %d candles)\n", ts, c.signal, active, c.maxAge)
		return
	}
	fmt.Printf("[%s] %d new %s:\n", ts, len(fresh), c.signal)
	for _, h := range fresh {
		fmt.Printf("    %-12s %-5s %-9s  level %-11s last %-11s (%d bars ago)%s\n",
			h.symbol, h.ev.Setup, bias(h.ev.Side),
			fmt.Sprintf("%.6g", h.ev.Level), fmt.Sprintf("%.6g", h.last), h.barsAgo, fvgNote(c, h))
	}
}

// findFVG looks for the first three-candle fair value gap in the break direction
// whose displacement (middle) candle falls within fvgWindow candles of the CHoCH
// — i.e. the imbalance left by the move that broke structure. It returns the gap
// zone and the index of the candle that completes it.
func findFVG(ks []kline.Kline, breakIdx int, side strategy.Side, window int) (lo, hi float64, formedIdx int, ok bool) {
	for m := breakIdx; m <= breakIdx+window && m+1 < len(ks); m++ {
		if m-1 < 0 {
			continue
		}
		if lo, hi, ok = structure.FVG(ks[m-1], ks[m+1], side); ok {
			return lo, hi, m + 1, true
		}
	}
	return 0, 0, 0, false
}

// zoneTapped reports whether any candle after the gap completed has traded back
// into the [lo, hi] zone — i.e. price already retraced into the FVG (an entry
// would have triggered), as opposed to the gap still sitting unfilled ahead.
func zoneTapped(ks []kline.Kline, formedIdx int, lo, hi float64) bool {
	for j := formedIdx + 1; j < len(ks); j++ {
		if ks[j].Low <= hi && ks[j].High >= lo {
			return true
		}
	}
	return false
}

// fvgNote renders the FVG zone and whether it has been tapped, for -fvg mode.
// "fresh" = price hasn't retraced into the gap yet (pending entry); "tapped" =
// price already entered the zone.
func fvgNote(c scanCfg, h hit) string {
	if !c.fvg {
		return ""
	}
	status := "fresh"
	if h.fvgTapped {
		status = "tapped"
	}
	return fmt.Sprintf("  FVG %.6g-%.6g (%s)", h.fvgLo, h.fvgHi, status)
}

// fvgZone is a fair value gap on the higher timeframe: its direction and price
// band. Bullish zone = [c1.High, c3.Low]; bearish = [c3.High, c1.Low].
type fvgZone struct {
	side   strategy.Side
	lo, hi float64
}

// collectUnfilledFVGs scans every three-candle window for a fair value gap (in
// either direction) and keeps only the UNFILLED ones — gaps no later candle has
// traded back into. Those are the still-active higher-timeframe imbalances.
func collectUnfilledFVGs(ks []kline.Kline) []fvgZone {
	var zones []fvgZone
	for m := 1; m+1 < len(ks); m++ {
		c1, c3 := ks[m-1], ks[m+1]
		for _, side := range []strategy.Side{strategy.Long, strategy.Short} {
			if lo, hi, ok := structure.FVG(c1, c3, side); ok {
				if !zoneTapped(ks, m+1, lo, hi) {
					zones = append(zones, fvgZone{side: side, lo: lo, hi: hi})
				}
				break // a triple forms at most one gap
			}
		}
	}
	return zones
}

// htfContradicts reports whether any unfilled HTF gap opposes the setup and lies
// in the trade's path within rng (fraction of price): for a long, a bearish HTF
// gap at or above price (overhead supply); for a short, a bullish HTF gap at or
// below price (support beneath). Price already inside such a gap counts too.
func htfContradicts(zones []fvgZone, setup strategy.Side, price, rng float64) bool {
	for _, z := range zones {
		if setup == strategy.Long {
			if z.side == strategy.Short && z.hi >= price && z.lo <= price*(1+rng) {
				return true
			}
		} else {
			if z.side == strategy.Long && z.lo <= price && z.hi >= price*(1-rng) {
				return true
			}
		}
	}
	return false
}

// want reports whether a setup should be reported under the -signal filter.
func want(signal, setup string) bool {
	switch signal {
	case "bos":
		return setup == "BOS"
	case "both":
		return true
	default: // "choch"
		return setup == "CHoCH"
	}
}

// bias renders a break direction as a trader-facing label.
func bias(side strategy.Side) string {
	if side == strategy.Short {
		return "bearish ↓"
	}
	return "bullish ↑" // long break = closed above a swing high
}

// intervalDuration parses a Binance interval ("1m","5m","1h","1d") to a Duration,
// defaulting to one minute on anything unrecognized.
func intervalDuration(iv string) time.Duration {
	if len(iv) < 2 {
		return time.Minute
	}
	n, err := strconv.Atoi(iv[:len(iv)-1])
	if err != nil || n <= 0 {
		return time.Minute
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
	return time.Minute
}

func report(c scanCfg, scanned, total, htfDropped int, hits []hit) {
	fmt.Printf("=== Structure screen (%s, %s, %s tier) ===\n", c.signal, c.interval, c.tier)
	fmt.Printf("Scanned %d/%d symbols; %d with a %s in the last %d candles.\n",
		scanned, total, len(hits), c.signal, c.maxAge)
	if c.htf != "" {
		fmt.Printf("HTF filter (%s): dropped %d setup(s) with a contradicting %s FVG within %.2f%%.\n",
			c.htf, htfDropped, c.htf, c.htfRange*100)
	}
	fmt.Println()
	if len(hits) == 0 {
		fmt.Println("No matches. Loosen the filters: larger -max-age/-top/-days, lower -swing-n, -min-trend-bars 0, or drop -fvg / raise -fvg-window.")
		return
	}
	fvgHdr := ""
	if c.fvg {
		fvgHdr = "  FVG zone (status)"
	}
	fmt.Printf("  %-14s %-9s %-9s %-8s %-20s %12s%s\n", "symbol", "setup", "bias", "bars ago", "broke level @ (UTC)", "last", fvgHdr)
	for _, h := range hits {
		fmt.Printf("  %-14s %-9s %-9s %-8d %-10s %-9s %12.6g%s\n",
			h.symbol, h.ev.Setup, bias(h.ev.Side), h.barsAgo,
			h.ev.Time.Format("15:04"), fmt.Sprintf("%.6g", h.ev.Level), h.last, fvgNote(c, h))
	}
	fmt.Printf("\nbias = direction of the break (bullish = reversal up / continuation up).\n")
	fmt.Printf("bars ago = closed candles since the break; 0 = the most recent candle.\n")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
