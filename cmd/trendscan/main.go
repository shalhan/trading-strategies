// Command trendscan is a multi-timeframe trendline screener for the
// breakout-and-retest strategy. For each symbol in a liquidity-filtered
// universe it fetches klines on every configured timeframe (default
// 1d,4h,1h,30m,5m,1m), detects the best support and resistance trendlines on
// each (the lines with the most touches that were never broken by a close),
// and reports every coin whose CURRENT price is at one of those lines:
//
//   - TOUCH:  price is at an intact line — a bounce or an imminent break.
//   - RETEST: the line was broken by a close within the last -recent-break
//     candles and price has come back to it — the breakout-retest entry.
//
// Touch/break tolerances are ATR multiples of each timeframe (ATR-based, not
// fixed %, per CLAUDE.md), so one setting behaves consistently across coins.
//
// One-shot by default: scan once and print the table. With -watch it re-scans
// at each close of the smallest timeframe and streams only NEW alerts (a
// status change per symbol/timeframe/line side), ringing the terminal bell;
// -notify additionally posts a macOS notification. Higher-timeframe klines are
// cached in memory and re-fetched only after that timeframe's candle closes,
// so a watch cycle costs roughly one request per symbol.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/dataset"
	"github.com/shalhan/orderflow-trading-app/internal/indicator"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/trendline"
	"github.com/shalhan/orderflow-trading-app/internal/universe"
)

// tfSpec is one timeframe to screen: its Binance interval name and duration.
// rank preserves the order given on the command line (highest timeframe first)
// so alerts can sort by significance.
type tfSpec struct {
	name string
	d    time.Duration
	rank int
}

// scanCfg bundles the per-scan parameters.
type scanCfg struct {
	dataDir     string
	tfs         []tfSpec
	bars        int // candles of history per timeframe fed to detection
	pivotN      int
	minTouches  int
	minSpan     int
	touchATR    float64 // touch tolerance as an ATR multiple
	breakATR    float64 // break confirmation as an ATR multiple
	recentBreak int     // bars a break may be old and still count as a retest setup
	atrPeriod   int
	minKlines   int
	workers     int
}

// alert is one symbol currently at a trendline on one timeframe.
type alert struct {
	symbol  string
	tf      tfSpec
	line    trendline.Line
	status  trendline.Status
	lineVal float64
	price   float64
	nowIdx  float64 // current fractional bar index in the timeframe's series
}

// barsSinceBreak is how many of this timeframe's candles ago the line broke.
func (a alert) barsSinceBreak() int {
	if a.line.BreakIdx < 0 {
		return 0
	}
	n := int(a.nowIdx) - a.line.BreakIdx
	if n < 0 {
		n = 0
	}
	return n
}

// key identifies the line slot the alert belongs to, for watch-mode dedupe.
func (a alert) key() string { return a.symbol + "|" + a.tf.name + "|" + a.line.Kind.String() }

func (a alert) distPct() float64 { return (a.price - a.lineVal) / a.price * 100 }

func main() {
	top := flag.Int("top", 50, "universe size: top-N USDT pairs by 24h volume")
	tfsFlag := flag.String("tfs", "1d,4h,1h,30m,5m,1m", "timeframes to build trendlines on, highest first")
	bars := flag.Int("bars", 300, "candles of history per timeframe used for line detection")
	pivotN := flag.Int("pivot-n", 3, "bars each side defining a swing pivot (line anchors)")
	minTouches := flag.Int("min-touches", 3, "minimum touches for a trendline to qualify")
	minSpan := flag.Int("min-span", 10, "minimum bars between a line's two anchor pivots")
	touchATR := flag.Float64("touch-atr", 0.25, "touch tolerance as a multiple of the timeframe's ATR")
	breakATR := flag.Float64("break-atr", 0.25, "a close this many ATRs beyond the line confirms a break")
	recentBreak := flag.Int("recent-break", 12, "a broken line counts as a retest setup only within this many candles of the break")
	atrPeriod := flag.Int("atr-period", 14, "ATR period for the tolerances")
	minKlines := flag.Int("min-klines", 60, "skip a symbol's timeframe with fewer than this many candles")
	workers := flag.Int("workers", 8, "concurrent symbols fetched per scan")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	watch := flag.Bool("watch", false, "stream continuously: re-scan each smallest-timeframe close and print only new alerts")
	notify := flag.Bool("notify", false, "with -watch: also post a macOS notification for each new alert")
	flag.Parse()

	tfs, err := parseTFs(*tfsFlag)
	if err != nil {
		fail("%v", err)
	}
	cfg := scanCfg{
		dataDir: *dataDir, tfs: tfs, bars: *bars, pivotN: *pivotN,
		minTouches: *minTouches, minSpan: *minSpan, touchATR: *touchATR,
		breakATR: *breakATR, recentBreak: *recentBreak, atrPeriod: *atrPeriod,
		minKlines: *minKlines, workers: *workers,
	}
	client := binance.NewClient()

	fmt.Printf("selecting top-%d USDT universe by 24h volume...\n", *top)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: *top})
	cancel()
	if err != nil {
		fail("select universe: %v", err)
	}

	cache := newTFCache()
	if *watch {
		watchLoop(client, syms, cfg, cache, *notify)
		return
	}

	fmt.Printf("scanning %d symbols on %s (bars %d, min-touches %d, touch %.2f ATR)...\n\n",
		len(syms), *tfsFlag, cfg.bars, cfg.minTouches, cfg.touchATR)
	report(cfg, scanOnce(client, syms, cfg, cache))
}

// scanOnce screens every symbol on every timeframe and returns the alerts,
// retests first, then higher timeframes, then closest to the line.
func scanOnce(client *binance.Client, syms []universe.Symbol, c scanCfg, cache *tfCache) []alert {
	now := time.Now().UTC()
	var (
		mu  sync.Mutex
		out []alert
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, c.workers)
	for _, s := range syms {
		wg.Add(1)
		sem <- struct{}{}
		go func(s universe.Symbol) {
			defer wg.Done()
			defer func() { <-sem }()
			hits := scanSymbol(client, s.Symbol, c, cache, now)
			if len(hits) == 0 {
				return
			}
			mu.Lock()
			out = append(out, hits...)
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool {
		if out[i].status != out[j].status {
			return out[i].status > out[j].status // Retest before Touch
		}
		if out[i].tf.rank != out[j].tf.rank {
			return out[i].tf.rank < out[j].tf.rank // higher timeframe first
		}
		di, dj := abs(out[i].distPct()), abs(out[j].distPct())
		if di != dj {
			return di < dj
		}
		return out[i].symbol < out[j].symbol
	})
	return out
}

// scanSymbol loads every timeframe for one symbol, detects its trendlines, and
// classifies the current price (the last closed candle of the smallest
// timeframe) against each line.
func scanSymbol(client *binance.Client, sym string, c scanCfg, cache *tfCache, now time.Time) []alert {
	series := make(map[string][]kline.Kline, len(c.tfs))
	for _, tf := range c.tfs {
		ks, err := cache.get(client, c, sym, tf, now)
		if err != nil || len(ks) < c.minKlines {
			continue
		}
		series[tf.name] = ks
	}

	// Current price = last closed candle of the smallest timeframe available.
	smallest := tfSpec{}
	for _, tf := range c.tfs {
		if _, ok := series[tf.name]; ok && (smallest.d == 0 || tf.d < smallest.d) {
			smallest = tf
		}
	}
	if smallest.d == 0 {
		return nil
	}
	sks := series[smallest.name]
	price := sks[len(sks)-1].Close

	var hits []alert
	for _, tf := range c.tfs {
		ks, ok := series[tf.name]
		if !ok {
			continue
		}
		atrVal, ready := atrOf(ks, c.atrPeriod)
		if !ready || atrVal <= 0 {
			continue
		}
		lines := trendline.Detect(ks, trendline.Config{
			PivotN: c.pivotN, MinTouches: c.minTouches, MinSpan: c.minSpan,
			TouchTol: c.touchATR * atrVal, BreakTol: c.breakATR * atrVal,
			RecentBreakBars: c.recentBreak,
		})
		// Project each line to "now" in this timeframe's bar units.
		nowIdx := float64(now.Sub(ks[0].OpenTime)) / float64(tf.d)
		for _, l := range lines {
			st, v := l.StatusAt(nowIdx, price, c.touchATR*atrVal)
			if st == trendline.None {
				continue
			}
			hits = append(hits, alert{symbol: sym, tf: tf, line: l, status: st, lineVal: v, price: price, nowIdx: nowIdx})
		}
	}
	return hits
}

// watchLoop re-scans at each close of the smallest timeframe and streams only
// alerts whose status changed since the last cycle (new touch, new retest, or
// touch upgraded to retest) — a line being sat on is reported once, not every
// minute.
func watchLoop(client *binance.Client, syms []universe.Symbol, c scanCfg, cache *tfCache, notify bool) {
	d := c.tfs[0].d
	for _, tf := range c.tfs {
		if tf.d < d {
			d = tf.d
		}
	}
	fmt.Printf("watching %d symbols on %s — alerting on trendline touches and breakout-retests (Ctrl-C to stop)\n\n",
		len(syms), tfNames(c.tfs))

	prev := map[string]trendline.Status{}
	for {
		hits := scanOnce(client, syms, c, cache)
		cur := make(map[string]trendline.Status, len(hits))
		var fresh []alert
		for _, h := range hits {
			cur[h.key()] = h.status
			if h.status != prev[h.key()] {
				fresh = append(fresh, h)
			}
		}
		prev = cur
		streamCycle(fresh, len(hits))
		if notify {
			notifyAlerts(fresh)
		}

		// Wake just after the next smallest-timeframe close so the fresh candle
		// is available over REST.
		now := time.Now()
		next := now.Truncate(d).Add(d + 4*time.Second)
		time.Sleep(time.Until(next))
	}
}

// streamCycle prints one watch cycle: the new alerts (with a terminal bell), or
// a heartbeat so the feed visibly stays alive.
func streamCycle(fresh []alert, active int) {
	ts := time.Now().Format("15:04:05")
	if len(fresh) == 0 {
		fmt.Printf("[%s] no new alerts (%d symbol-lines currently touching)\n", ts, active)
		return
	}
	fmt.Printf("\a[%s] %d new alert(s):\n", ts, len(fresh))
	for _, h := range fresh {
		fmt.Printf("    %s\n", h.render())
	}
}

// render formats one alert as a single trader-facing line.
func (a alert) render() string {
	broke := ""
	if a.line.BreakIdx >= 0 {
		broke = fmt.Sprintf("  broke %d %s candles ago", a.barsSinceBreak(), a.tf.name)
	}
	return fmt.Sprintf("%-12s %-4s %-10s %-6s line %-11s price %-11s (%+.2f%%)  touches %d  slope %s%s",
		a.symbol, a.tf.name, a.line.Kind, a.status,
		fmt.Sprintf("%.6g", a.lineVal), fmt.Sprintf("%.6g", a.price),
		a.distPct(), a.line.Touches, slopeArrow(a.line.Slope), broke)
}

// notifyAlerts posts a macOS notification per alert (best-effort, darwin only).
func notifyAlerts(fresh []alert) {
	if runtime.GOOS != "darwin" {
		return
	}
	for _, h := range fresh {
		msg := fmt.Sprintf("%s %s %s %s @ %.6g (price %.6g)",
			h.symbol, h.tf.name, h.line.Kind, h.status, h.lineVal, h.price)
		cmd := exec.Command("osascript", "-e",
			fmt.Sprintf("display notification %q with title %q sound name %q", msg, "trendscan", "Ping"))
		_ = cmd.Run()
	}
}

func report(c scanCfg, hits []alert) {
	fmt.Printf("=== Trendline screen (%s) ===\n", tfNames(c.tfs))
	fmt.Printf("%d symbol-timeframe lines currently touching (RETEST = broken then revisited — the entry setup).\n\n", len(hits))
	if len(hits) == 0 {
		fmt.Println("No coins at a trendline right now. Loosen with a larger -touch-atr, lower -min-touches, or larger -top.")
		return
	}
	fmt.Printf("  %-12s %-4s %-10s %-6s %-12s %-12s %-8s %-7s %-5s %s\n",
		"symbol", "tf", "kind", "status", "line", "price", "dist%", "touches", "slope", "broke")
	for _, h := range hits {
		broke := "-"
		if h.line.BreakIdx >= 0 {
			broke = fmt.Sprintf("%d bars ago", h.barsSinceBreak())
		}
		fmt.Printf("  %-12s %-4s %-10s %-6s %-12s %-12s %+-8.2f %-7d %-5s %s\n",
			h.symbol, h.tf.name, h.line.Kind, h.status,
			fmt.Sprintf("%.6g", h.lineVal), fmt.Sprintf("%.6g", h.price),
			h.distPct(), h.line.Touches, slopeArrow(h.line.Slope), broke)
	}
	fmt.Printf("\ndist%% = price vs line (+ above / - below). touches = distinct tests of the line.\n")
	fmt.Printf("RETEST rows are your breakout-and-retest candidates; TOUCH rows are lines to watch for the break.\n")
}

// --- in-memory kline cache: one fetch per timeframe candle ---

// tfCache holds each (symbol, timeframe) series and re-fetches only once that
// timeframe's current candle boundary advances, so watch cycles cost ~one
// request per symbol (the smallest timeframe) instead of one per timeframe.
type tfCache struct {
	mu sync.Mutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	boundary time.Time
	ks       []kline.Kline
}

func newTFCache() *tfCache { return &tfCache{m: map[string]cacheEntry{}} }

func (tc *tfCache) get(client *binance.Client, c scanCfg, sym string, tf tfSpec, now time.Time) ([]kline.Kline, error) {
	key := sym + "|" + tf.name
	boundary := now.Truncate(tf.d)
	tc.mu.Lock()
	if e, ok := tc.m[key]; ok && e.boundary.Equal(boundary) {
		tc.mu.Unlock()
		return e.ks, nil
	}
	tc.mu.Unlock()

	// Fetch bars + ATR warmup, plus slack for gaps/new listings.
	start := now.Add(-tf.d * time.Duration(c.bars+c.atrPeriod+10))
	ks, err := dataset.LoadKlines(context.Background(), client, c.dataDir, sym, tf.name, start, now, true)
	if err != nil {
		return nil, err
	}
	// Drop a still-forming final candle: every line and touch must be on closes.
	for len(ks) > 0 && ks[len(ks)-1].CloseTime.After(now) {
		ks = ks[:len(ks)-1]
	}
	if len(ks) > c.bars {
		ks = ks[len(ks)-c.bars:]
	}
	tc.mu.Lock()
	tc.m[key] = cacheEntry{boundary: boundary, ks: ks}
	tc.mu.Unlock()
	return ks, nil
}

// --- helpers ---

// atrOf runs the streaming Wilder ATR over the series and returns its final value.
func atrOf(ks []kline.Kline, period int) (float64, bool) {
	a := indicator.NewATR(period)
	var v float64
	var ready bool
	for _, k := range ks {
		v, ready = a.Update(k)
	}
	return v, ready
}

func parseTFs(s string) ([]tfSpec, error) {
	var out []tfSpec
	for i, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		d, err := intervalDuration(name)
		if err != nil {
			return nil, fmt.Errorf("timeframe %q: %w", name, err)
		}
		out = append(out, tfSpec{name: name, d: d, rank: i})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no timeframes given")
	}
	return out, nil
}

// intervalDuration parses a Binance interval ("1m","30m","4h","1d") to a Duration.
func intervalDuration(iv string) (time.Duration, error) {
	if len(iv) < 2 {
		return 0, fmt.Errorf("unrecognized interval")
	}
	n, err := strconv.Atoi(iv[:len(iv)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("unrecognized interval")
	}
	switch iv[len(iv)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unrecognized interval")
}

func tfNames(tfs []tfSpec) string {
	names := make([]string, len(tfs))
	for i, tf := range tfs {
		names[i] = tf.name
	}
	return strings.Join(names, ",")
}

func slopeArrow(s float64) string {
	switch {
	case s > 0:
		return "↑"
	case s < 0:
		return "↓"
	}
	return "→"
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
