// Command paper runs the validated setup ("config 08") as a LIVE paper trader:
// the real structure engines over a frozen top-N universe, mediated by the real
// portfolio risk layer, stepped at each candle close from live Binance klines.
// No orders are sent anywhere — fills are simulated exactly like the backtest
// (limit fills on touch, stops/exits on the closed candle), costs are applied
// per trade (maker entry for FVG limits, taker exit, slippage), and every
// decision is logged so live behavior can be compared with the backtest.
//
// Accounting is linear, matching the walk-forward: 1R = -risk × starting
// capital; equity = capital + Σ(netR × riskUSD). Funding is NOT modeled — that
// is one of the two things this paper run exists to observe (the other is
// whether touched limits would really fill).
//
// State: the universe is frozen in <state>/universe.json on first start (delete
// it to re-select). On (re)start the engines replay -warmup-bars of history to
// rebuild structure, with all entries suppressed — the paper account always
// starts flat; a restart drops simulated open positions (logged).
//
// Files under -state (default ./paper-state): universe.json, trades.ndjson (closed
// trades with $), events.ndjson (entries/skips/cancels), status.json (equity,
// open positions — refreshed each cycle). Run with -report to print a summary
// of the logs and exit.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

type app struct {
	cfgEngine structure.Config
	interval  string
	d         time.Duration
	capital   float64
	riskFrac  float64
	costs     backtest.Costs
	stateDir  string
	dataDir   string
	notify    bool
	discord   string
	tgToken   string
	tgChat    string

	client  *binance.Client
	mgr     *portfolio.Manager
	engines map[string]*structure.Engine
	lastFed map[string]time.Time
	started time.Time
	netR    float64
	closed  int
	wins    int
}

func main() {
	capital := flag.Float64("capital", 260, "paper account size in USD")
	risk := flag.Float64("risk", 0.01, "risk per trade (fraction of starting capital)")
	maxConc := flag.Int("max-concurrent", 5, "max simultaneous positions")
	top := flag.Int("top", 20, "universe size (frozen on first start)")
	interval := flag.String("interval", "1h", "signal timeframe")
	warmupBars := flag.Int("warmup-bars", 1500, "history bars replayed on start to rebuild structure (must cover weeks so the daily HTF trend is established)")
	stateDir := flag.String("state", "./paper-state", "state/log directory")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	feeRate := flag.Float64("fee-rate", 0.0005, "taker fee per side")
	makerFee := flag.Float64("maker-fee", 0.0002, "maker fee (FVG limit entries)")
	slipRate := flag.Float64("slip-rate", 0.0002, "slippage per side")
	notify := flag.Bool("notify", false, "macOS notification on entries/exits")
	discord := flag.String("discord-webhook", os.Getenv("DISCORD_WEBHOOK"), "Discord webhook URL for entry/exit alerts (env DISCORD_WEBHOOK)")
	tgToken := flag.String("telegram-token", os.Getenv("TELEGRAM_TOKEN"), "Telegram bot token (env TELEGRAM_TOKEN)")
	tgChat := flag.String("telegram-chat", os.Getenv("TELEGRAM_CHAT"), "Telegram chat id (env TELEGRAM_CHAT)")
	addr := flag.String("addr", ":8899", "HTTP status listen address (empty = off)")
	report := flag.Bool("report", false, "print a summary of the logs and exit")
	flag.Parse()

	if *report {
		printReport(*stateDir, *capital, *risk)
		return
	}

	d := map[string]time.Duration{"15m": 15 * time.Minute, "30m": 30 * time.Minute,
		"1h": time.Hour, "4h": 4 * time.Hour}[*interval]
	if d == 0 {
		fail("unsupported interval %q (15m/30m/1h/4h)", *interval)
	}
	if err := os.MkdirAll(*stateDir, 0o755); err != nil {
		fail("state dir: %v", err)
	}

	a := &app{
		// Config 08 — the tuned, twice-OOS-validated setup. Deliberately not
		// flag-tunable here: the paper test must test what was validated.
		cfgEngine: structure.Config{
			PivotN: 3, ATRPeriod: 14, TrendModel: "pivot", SwingDevATR: 1,
			StrictCHoCH: true, Signals: "both",
			UseFVG: true, FVGLookback: 5, // edge entry (no midpoint)
			MaxStopATR: 3, MinStopFrac: 0.0005, TargetR: 4,
			RTrail: true, RTrailStart: 1, RTrailStep: 1, RTrailOffset: 1,
			BlackoutSessions: "asia,london,us", SessionBufMin: 30,
			HTFAlign: true, HTFPeriod: 24 * time.Hour, HTFPivotN: 3, // 08b: skip counter-daily-trend entries
		},
		interval: *interval, d: d, capital: *capital, riskFrac: *risk,
		costs:    backtest.Costs{FeeRate: *feeRate, MakerFee: *makerFee, SlipRate: *slipRate},
		stateDir: *stateDir, dataDir: *dataDir, notify: *notify,
		discord: *discord, tgToken: *tgToken, tgChat: *tgChat,
		client:  binance.NewClient(),
		mgr:     portfolio.New(portfolio.Config{Capital: *capital, RiskPerTrade: *risk, MaxConcurrent: *maxConc, MaxTotalRisk: float64(*maxConc) * *risk}),
		engines: map[string]*structure.Engine{},
		lastFed: map[string]time.Time{},
	}

	syms, err := a.universeSyms(*top)
	if err != nil {
		fail("universe: %v", err)
	}
	for _, s := range syms {
		ecfg := a.cfgEngine
		ecfg.Symbol = s
		a.engines[s] = structure.New(ecfg)
	}

	fmt.Printf("paper account $%.2f, risk %.2g%%/trade ($%.2f = 1R), %d symbols on %s\n",
		*capital, *risk*100, *capital**risk, len(syms), *interval)
	fmt.Printf("config 08b: pivot·dev1·strict-CHoCH·FVG-edge(lb5)·R-trail 1/+1/−1·no-session±30m·min-stop 0.05%%·max-stop 3ATR·HTF-align 1d\n")
	fmt.Printf("warming up structure on %d bars (entries suppressed)...\n", *warmupBars)
	a.replay(*warmupBars)
	a.started = time.Now().UTC()
	a.logEvent(map[string]any{"t": a.started, "type": "start", "symbols": syms, "capital": *capital})
	fmt.Printf("armed at %s — paper trading live (Ctrl-C to stop; restart replays structure but starts flat)\n\n",
		a.started.Format("2006-01-02 15:04 UTC"))
	a.writeStatus()
	if *addr != "" {
		go a.serveStatus(*addr)
	}
	a.notifyMsg(fmt.Sprintf("paper trader armed: $%.2f, %d symbols, %s", *capital, len(syms), *interval))

	for {
		next := time.Now().Truncate(a.d).Add(a.d + 5*time.Second)
		time.Sleep(time.Until(next))
		a.cycle(false)
		a.writeStatus()
	}
}

// universeSyms freezes the universe on first start and reuses it afterwards.
func (a *app) universeSyms(top int) ([]string, error) {
	path := filepath.Join(a.stateDir, "universe.json")
	if b, err := os.ReadFile(path); err == nil {
		var syms []string
		if json.Unmarshal(b, &syms) == nil && len(syms) > 0 {
			fmt.Printf("reusing frozen universe (%d symbols) from %s\n", len(syms), path)
			return syms, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	us, err := universe.Fetch(ctx, a.client, universe.Options{QuoteAsset: "USDT", TopN: top})
	if err != nil {
		return nil, err
	}
	syms := make([]string, len(us))
	for i, u := range us {
		syms[i] = u.Symbol
	}
	b, _ := json.Marshal(syms)
	_ = os.WriteFile(path, b, 0o644)
	return syms, nil
}

// replay feeds history through the engines to rebuild structure. Entries are
// suppressed (every proposal rejected) so the paper account starts flat.
func (a *app) replay(bars int) {
	now := time.Now().UTC()
	start := now.Add(-a.d * time.Duration(bars))
	for sym, eng := range a.engines {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		ks, err := dataset.LoadKlines(ctx, a.client, a.dataDir, sym, a.interval, start, now, true)
		cancel()
		if err != nil {
			fmt.Printf("  warmup %s: %v (will retry live)\n", sym, err)
			continue
		}
		for _, k := range ks {
			if k.CloseTime.After(now) {
				break
			}
			res := eng.Step(k)
			if res.Proposal != nil {
				eng.Resolve(false)
			}
			a.lastFed[sym] = k.OpenTime
		}
	}
}

// cycle fetches fresh closed candles for every symbol and steps them through
// the engines and the portfolio in bar-time order.
func (a *app) cycle(quiet bool) {
	now := time.Now().UTC()
	type bar struct {
		sym string
		k   kline.Kline
	}
	var bars []bar
	for sym := range a.engines {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		ks, err := dataset.LoadKlines(ctx, a.client, a.dataDir, sym, a.interval,
			a.lastFed[sym].Add(-2*a.d), now, true)
		cancel()
		if err != nil {
			fmt.Printf("[%s] fetch %s: %v\n", now.Format("15:04"), sym, err)
			continue
		}
		for _, k := range ks {
			if k.OpenTime.After(a.lastFed[sym]) && !k.CloseTime.After(now) {
				bars = append(bars, bar{sym, k})
			}
		}
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].k.OpenTime.Before(bars[j].k.OpenTime) })

	// Group by bar time so competing proposals are admitted together, like the
	// backtest does.
	for i := 0; i < len(bars); {
		j := i
		var proposals []*strategy.Proposal
		byolder := map[string]*structure.Engine{}
		for ; j < len(bars) && bars[j].k.OpenTime.Equal(bars[i].k.OpenTime); j++ {
			b := bars[j]
			eng := a.engines[b.sym]
			res := eng.Step(b.k)
			a.lastFed[b.sym] = b.k.OpenTime
			if res.Closed != nil {
				a.settle(res.Closed)
			}
			if res.Proposal != nil {
				proposals = append(proposals, res.Proposal)
				byolder[b.sym] = eng
			}
		}
		if len(proposals) > 0 {
			decision := a.mgr.Admit(proposals)
			for sym, eng := range byolder {
				eng.Resolve(decision[sym])
				a.logDecision(sym, eng, decision[sym], proposals)
			}
		}
		i = j
	}
}

func (a *app) settle(t *strategy.Trade) {
	a.mgr.Settle(t)
	net := a.costs.Apply([]*strategy.Trade{t})[0]
	riskUSD := a.capital * a.riskFrac
	a.netR += net.R
	a.closed++
	if net.R > 0 {
		a.wins++
	}
	usd := net.R * riskUSD
	rec := map[string]any{
		"t": t.ExitTime, "type": "close", "symbol": t.Symbol, "side": t.Side.String(),
		"setup": t.Setup, "entryT": t.EntryTime, "entry": t.EntryPrice, "exit": t.ExitPrice,
		"outcome": string(t.Outcome), "grossR": t.R, "netR": net.R, "usd": usd,
		"equity": a.capital + a.netR*riskUSD,
	}
	a.append("trades.ndjson", rec)
	fmt.Printf("\a[%s] CLOSED %s %s %s  %s  net %+0.2fR ($%+.2f)  equity $%.2f\n",
		time.Now().Format("15:04"), t.Symbol, t.Side, t.Setup, t.Outcome, net.R, usd,
		a.capital+a.netR*riskUSD)
	a.notifyMsg(fmt.Sprintf("CLOSED %s %s %+0.2fR ($%+.2f)", t.Symbol, t.Side, net.R, usd))
}

func (a *app) logDecision(sym string, eng *structure.Engine, accepted bool, _ []*strategy.Proposal) {
	if !accepted {
		a.append("events.ndjson", map[string]any{"t": time.Now().UTC(), "type": "portfolio_reject", "symbol": sym})
		fmt.Printf("[%s] portfolio rejected %s (caps full)\n", time.Now().Format("15:04"), sym)
		return
	}
	op := eng.OpenPosition()
	if op == nil {
		return
	}
	riskUSD := a.capital * a.riskFrac
	notional := 0.0
	if op.StopDist > 0 {
		notional = riskUSD / (op.StopDist / op.Entry)
	}
	a.append("events.ndjson", map[string]any{
		"t": time.Now().UTC(), "type": "open", "symbol": sym, "side": op.Side.String(),
		"entry": op.Entry, "stop": op.Stop, "notionalUSD": notional,
	})
	fmt.Printf("\a[%s] OPEN %s %s @ %.6g stop %.6g  (risk $%.2f, notional $%.0f)\n",
		time.Now().Format("15:04"), sym, op.Side, op.Entry, op.Stop, riskUSD, notional)
	a.notifyMsg(fmt.Sprintf("OPEN %s %s @ %.6g", sym, op.Side, op.Entry))
}

func (a *app) writeStatus() {
	type pos struct {
		Symbol string  `json:"symbol"`
		Side   string  `json:"side"`
		Entry  float64 `json:"entry"`
		Stop   float64 `json:"stop"`
	}
	var open []pos
	for sym, eng := range a.engines {
		if p := eng.OpenPosition(); p != nil {
			open = append(open, pos{sym, p.Side.String(), p.Entry, p.Stop})
		}
	}
	riskUSD := a.capital * a.riskFrac
	b, _ := json.MarshalIndent(map[string]any{
		"updated": time.Now().UTC(), "started": a.started,
		"capital": a.capital, "riskUSD": riskUSD,
		"closedTrades": a.closed, "wins": a.wins, "netR": a.netR,
		"equity": a.capital + a.netR*riskUSD, "open": open,
	}, "", "  ")
	_ = os.WriteFile(filepath.Join(a.stateDir, "status.json"), b, 0o644)
}

func (a *app) append(file string, v any) {
	f, err := os.OpenFile(filepath.Join(a.stateDir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(v)
	f.Write(append(b, '\n'))
}

func (a *app) logEvent(v map[string]any) { a.append("events.ndjson", v) }

func (a *app) notifyMsg(msg string) {
	if a.notify && runtime.GOOS == "darwin" {
		_ = exec.Command("osascript", "-e",
			fmt.Sprintf("display notification %q with title %q sound name %q", msg, "paper", "Ping")).Run()
	}
	if a.discord != "" {
		body, _ := json.Marshal(map[string]string{"content": "📈 " + msg})
		resp, err := http.Post(a.discord, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}
	if a.tgToken != "" && a.tgChat != "" {
		resp, err := http.PostForm("https://api.telegram.org/bot"+a.tgToken+"/sendMessage",
			url.Values{"chat_id": {a.tgChat}, "text": {"📈 " + msg}})
		if err == nil {
			resp.Body.Close()
		}
	}
}

// serveStatus exposes the state files over HTTP: /status (JSON), /trades
// (ndjson), /log (recent log lines are in the journal on servers).
func (a *app) serveStatus(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.ServeFile(w, r, filepath.Join(a.stateDir, "status.json"))
	})
	mux.HandleFunc("/trades", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(a.stateDir, "trades.ndjson"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "paper trader — endpoints: /status /trades")
	})
	_ = http.ListenAndServe(addr, mux)
}

func printReport(stateDir string, capital, risk float64) {
	b, err := os.ReadFile(filepath.Join(stateDir, "status.json"))
	if err != nil {
		fmt.Println("no status yet — is the paper trader running? (start: go run ./cmd/paper)")
		return
	}
	fmt.Println(string(b))
	f, err := os.Open(filepath.Join(stateDir, "trades.ndjson"))
	if err != nil {
		fmt.Println("\nno closed trades yet")
		return
	}
	defer f.Close()
	fmt.Println("\nclosed trades:")
	dec := json.NewDecoder(f)
	for {
		var t map[string]any
		if dec.Decode(&t) != nil {
			break
		}
		fmt.Printf("  %v %-12v %-5v %-6v %-7v net %+0.2fR ($%+.2f)\n",
			t["t"], t["symbol"], t["side"], t["setup"], t["outcome"], t["netR"], t["usd"])
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
