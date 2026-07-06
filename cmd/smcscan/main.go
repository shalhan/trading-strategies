// Command smcscan is the CLI front of internal/screener: a multi-symbol SMC
// screener with confidence scoring (see that package for the criteria). It
// scans a liquidity-filtered universe of USDT pairs and reports scored setups;
// alerts fire on CLOSED candles only:
//
//	SETUP   — a break closed and a limit is resting in the FVG
//	TRIGGER — the resting limit filled: actionable now
//	ENTER   — market-entry mode (-fvg=false): actionable at the break close
//
// One-shot by default (setups from the last -recent bars plus limits still
// resting); -watch re-scans at each candle close and streams alerts scoring
// ≥ -min-score (terminal bell; -notify adds a macOS notification). The same
// candidates appear in cmd/viz's scan panel, which can draw each one on the
// chart.
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
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/screener"
	"github.com/shalhan/orderflow-trading-app/internal/structure"
	"github.com/shalhan/orderflow-trading-app/internal/universe"
)

func main() {
	top := flag.Int("top", 50, "universe size: top-N USDT pairs by 24h volume")
	interval := flag.String("interval", "4h", "signal timeframe")
	bars := flag.Int("bars", 600, "candles of history per symbol")
	htf := flag.String("htf", "1d", "higher timeframe for the trend-alignment score (off = exclude the criterion)")
	htfPivotN := flag.Int("htf-pivot-n", 3, "swing strength for the HTF trend")
	minScore := flag.Int("min-score", 70, "alert threshold (0 = alert every setup)")
	recent := flag.Int("recent", 10, "one-shot: report setups from the last N closed bars")
	workers := flag.Int("workers", 8, "concurrent symbols per scan")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	watch := flag.Bool("watch", false, "stream continuously: re-scan each candle close, print only new alerts")
	notify := flag.Bool("notify", false, "with -watch: post a macOS notification per alert")

	w := screener.DefaultWeights()
	flag.IntVar(&w.HTF, "w-htf", w.HTF, "score weight: HTF trend alignment")
	flag.IntVar(&w.Setup, "w-setup", w.Setup, "score weight: setup type (CHoCH full, BOS half)")
	flag.IntVar(&w.FVG, "w-fvg", w.FVG, "score weight: unmitigated FVG entry zone")
	flag.IntVar(&w.Stop, "w-stop", w.Stop, "score weight: stop geometry")
	flag.IntVar(&w.Impulse, "w-impulse", w.Impulse, "score weight: impulse strength")

	trendModel := flag.String("trend-model", "leg", "structure model: leg | lux | pivot")
	swingDev := flag.Float64("swing-dev", 1, "ZigZag swing threshold in ATRs (0 = pivot-n)")
	pivotN := flag.Int("pivot-n", 3, "pivot strength when swing-dev is 0")
	luxLen := flag.Int("lux-len", 50, "lux model leg length")
	signals := flag.String("signals", "both", "both | bos | choch")
	useFVG := flag.Bool("fvg", true, "FVG limit entries (false = market at break close)")
	fvgMid := flag.Bool("fvg-mid", true, "limit at the gap midpoint instead of its edge")
	fvgLookback := flag.Int("fvg-lookback", 5, "bars back an unmitigated FVG may serve a break")
	maxStopATR := flag.Float64("max-stop-atr", 3, "skip setups with a stop wider than this many ATRs")
	minStopPct := flag.Float64("min-stop-pct", 0, "skip setups with a stop tighter than this % of price")
	targetR := flag.Float64("target-r", 4, "target in R")
	sessions := flag.String("block-sessions", "", "block entries around session opens/closes: comma list of asia,london,us (empty = off)")
	sessionBuf := flag.Int("session-buf", 30, "session blackout half-width in minutes")
	flag.Parse()

	d, err := intervalDuration(*interval)
	if err != nil {
		fail("interval: %v", err)
	}
	cfg := screener.Config{
		Interval: *interval, D: d, Bars: *bars, HTFPivotN: *htfPivotN, Workers: *workers, W: w,
		Engine: structure.Config{
			PivotN: *pivotN, ATRPeriod: 14, MaxStopATR: *maxStopATR, MinStopFrac: *minStopPct / 100,
			TargetR: *targetR, Signals: *signals, TrendModel: *trendModel, SwingDevATR: *swingDev,
			LuxLen: *luxLen, UseFVG: *useFVG, FVGMidpoint: *fvgMid, FVGLookback: *fvgLookback,
			BlackoutSessions: *sessions, SessionBufMin: *sessionBuf,
		},
	}
	if *htf != "off" && *htf != "" {
		hd, err := intervalDuration(*htf)
		if err != nil {
			fail("htf: %v", err)
		}
		cfg.HTFD = hd
	}

	client := binance.NewClient()
	fmt.Printf("selecting top-%d USDT universe by 24h volume...\n", *top)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: *top})
	cancel()
	if err != nil {
		fail("select universe: %v", err)
	}
	names := make([]string, len(syms))
	for i, s := range syms {
		names[i] = s.Symbol
	}
	sc := screener.New(cfg, names)

	if *watch {
		watchLoop(sc, client, cfg, *dataDir, *minScore, *notify, len(names))
		return
	}

	fmt.Printf("scanning %d symbols on %s (%s model, HTF %s, min score %d)...\n\n",
		len(names), cfg.Interval, cfg.Engine.TrendModel, *htf, *minScore)
	var all []screener.Candidate
	sc.Scan(context.Background(), client, *dataDir, func(c screener.Candidate) { all = append(all, c) })
	report(cfg, *recent, *minScore, sc.Resting(), all)
}

func report(cfg screener.Config, recent, minScore int, resting, all []screener.Candidate) {
	cutoff := time.Now().UTC().Add(-cfg.D * time.Duration(recent))
	seen := map[string]bool{}
	key := func(r screener.Candidate) string {
		return fmt.Sprintf("%s|%s|%d|%.8g", r.Symbol, r.Side, r.Time.Unix(), r.Entry)
	}
	rows := resting
	for _, r := range resting {
		seen[key(r)] = true
	}
	for _, c := range all {
		if c.Time.After(cutoff) && !seen[key(c)] {
			rows = append(rows, c)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].Time.After(rows[j].Time)
	})
	fmt.Printf("=== SMC screen: %d setups (last %d %s bars + resting limits; alert ≥ %d) ===\n\n",
		len(rows), recent, cfg.Interval, minScore)
	if len(rows) == 0 {
		fmt.Println("No fresh setups. Loosen with -recent, -min-score, or a larger -top.")
		return
	}
	fmt.Printf("  %-12s %-12s %-7s %-5s %-6s %5s  %-11s %-11s %-11s %-7s %s\n",
		"symbol", "time (UTC)", "stage", "side", "setup", "score", "entry", "stop", "target", "state", "breakdown")
	for _, r := range rows {
		state := "-"
		if r.Resting {
			state = "RESTING"
		}
		mark := "  "
		if r.Score >= minScore {
			mark = "! "
		}
		fmt.Printf("%s%-12s %-12s %-7s %-5s %-6s %5d  %-11s %-11s %-11s %-7s %s\n",
			mark, r.Symbol, r.Time.Format("01-02 15:04"), r.Stage, r.Side, r.Setup, r.Score,
			fmt.Sprintf("%.6g", r.Entry), fmt.Sprintf("%.6g", r.Stop), fmt.Sprintf("%.6g", r.Target),
			state, r.Parts)
	}
	fmt.Printf("\n'!' = at/above alert threshold. SETUP = limit resting; TRIGGER = limit filled (actionable); ENTER = market-mode signal.\n")
}

func watchLoop(sc *screener.Scanner, client *binance.Client, cfg screener.Config, dataDir string, minScore int, notify bool, n int) {
	fmt.Printf("arming %d symbols on %s (%s model)...\n", n, cfg.Interval, cfg.Engine.TrendModel)
	sc.Scan(context.Background(), client, dataDir, func(screener.Candidate) {}) // history: no alerts
	fmt.Printf("armed — alerting on setups scoring ≥ %d (Ctrl-C to stop)\n\n", minScore)

	for {
		next := time.Now().Truncate(cfg.D).Add(cfg.D + 4*time.Second)
		time.Sleep(time.Until(next))

		var fresh []screener.Candidate
		sc.Scan(context.Background(), client, dataDir, func(c screener.Candidate) {
			if c.Score >= minScore {
				fresh = append(fresh, c)
			}
		})
		ts := time.Now().Format("15:04:05")
		if len(fresh) == 0 {
			fmt.Printf("[%s] no new alerts\n", ts)
			continue
		}
		sort.Slice(fresh, func(i, j int) bool { return fresh[i].Score > fresh[j].Score })
		fmt.Printf("\a[%s] %d alert(s):\n", ts, len(fresh))
		for _, f := range fresh {
			fmt.Printf("    %-8s %-12s %-5s %-6s score %-3d  entry %.6g  stop %.6g  target %.6g  (%s)\n",
				f.Stage, f.Symbol, f.Side, f.Setup, f.Score, f.Entry, f.Stop, f.Target, f.Parts)
			if notify {
				notifyAlert(f)
			}
		}
	}
}

func notifyAlert(f screener.Candidate) {
	if runtime.GOOS != "darwin" {
		return
	}
	msg := fmt.Sprintf("%s %s %s %s score %d @ %.6g", f.Stage, f.Symbol, f.Side, f.Setup, f.Score, f.Entry)
	cmd := exec.Command("osascript", "-e",
		fmt.Sprintf("display notification %q with title %q sound name %q", msg, "smcscan", "Ping"))
	_ = cmd.Run()
}

func intervalDuration(iv string) (time.Duration, error) {
	if len(iv) < 2 {
		return 0, fmt.Errorf("unrecognized interval %q", iv)
	}
	n, err := strconv.Atoi(iv[:len(iv)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("unrecognized interval %q", iv)
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
	return 0, fmt.Errorf("unrecognized interval %q", iv)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
