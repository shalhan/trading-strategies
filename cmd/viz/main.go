// Command viz is a visual verifier for the structure (SMC) engine. It serves a
// local web page with a candlestick chart; pick a symbol/timeframe and press
// Start, and the server replays the REAL engine (internal/structure — the same
// code the backtests run) over recent klines and returns every decision it
// made: swing pivots, BOS/CHoCH breaks, FVG zones and resting limits, entries,
// stop moves, exits, and the skips (breaks that did NOT become trades, with the
// reason). The chart draws them so the implementation can be checked against
// intent, one setup at a time.
//
// Single-symbol and ungated: every proposal is accepted (no portfolio layer),
// and reported R is gross (no fees/slippage) — this is a logic verifier, not a
// backtest.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/dataset"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/screener"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
	"github.com/shalhan/orderflow-trading-app/internal/structure"
	"github.com/shalhan/orderflow-trading-app/internal/universe"
)

//go:embed index.html
var indexHTML []byte

var intervals = map[string]time.Duration{
	"1m": time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute,
	"30m": 30 * time.Minute, "1h": time.Hour, "4h": 4 * time.Hour, "1d": 24 * time.Hour,
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address")
	dataDir := flag.String("datadir", "./data", "kline cache directory")
	flag.Parse()

	client := binance.NewClient()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	http.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		if err := handleRun(w, r, client, *dataDir); err != nil {
			log.Printf("run: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
	})
	http.HandleFunc("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		if err := handleScan(w, r, client, *dataDir); err != nil {
			log.Printf("scan: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
	})
	log.Printf("viz listening on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// ---- API types (times are unix seconds, keyed to candle OPEN time) ----

type candleJSON struct {
	T int64   `json:"t"`
	O float64 `json:"o"`
	H float64 `json:"h"`
	L float64 `json:"l"`
	C float64 `json:"c"`
	V float64 `json:"v"`
}

type eventJSON struct {
	Type   string  `json:"type"`
	T      int64   `json:"t"`
	T2     int64   `json:"t2,omitempty"`
	Side   string  `json:"side,omitempty"`
	Setup  string  `json:"setup,omitempty"`
	Price  float64 `json:"price,omitempty"`
	Level  float64 `json:"level,omitempty"`
	Lo     float64 `json:"lo,omitempty"`
	Hi     float64 `json:"hi,omitempty"`
	Stop   float64 `json:"stop,omitempty"`
	Target float64 `json:"target,omitempty"`
	R      float64 `json:"r,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

type tradeJSON struct {
	Side     string  `json:"side"`
	Setup    string  `json:"setup"`
	EntryT   int64   `json:"entryT"`
	Entry    float64 `json:"entry"`
	InitStop float64 `json:"initStop"`
	Stop     float64 `json:"stop"` // final stop at exit (after any trailing)
	Target   float64 `json:"target"`
	ExitT    int64   `json:"exitT"`
	Exit     float64 `json:"exit"`
	Outcome  string  `json:"outcome"`
	R        float64 `json:"r"`
}

type openJSON struct {
	Side   string  `json:"side"`
	Setup  string  `json:"setup"`
	EntryT int64   `json:"entryT"`
	Entry  float64 `json:"entry"`
	Stop   float64 `json:"stop"`
	Target float64 `json:"target"`
}

type runResp struct {
	Symbol   string         `json:"symbol"`
	Interval string         `json:"interval"`
	Config   map[string]any `json:"config"`
	Candles  []candleJSON   `json:"candles"`
	Events   []eventJSON    `json:"events"`
	Trades   []tradeJSON    `json:"trades"`
	Open     *openJSON      `json:"open,omitempty"`
	NetR     float64        `json:"netR"`
}

func handleRun(w http.ResponseWriter, r *http.Request, client *binance.Client, dataDir string) error {
	q := r.URL.Query()
	symbol := strings.ToUpper(strings.TrimSpace(q.Get("symbol")))
	if symbol == "" {
		symbol = "BTCUSDT"
	}
	interval := q.Get("interval")
	if interval == "" {
		interval = "4h"
	}
	d, ok := intervals[interval]
	if !ok {
		return fmt.Errorf("unsupported interval %q", interval)
	}
	bars := qInt(q.Get("bars"), 600)
	if bars < 100 {
		bars = 100
	}
	if bars > 3000 {
		bars = 3000
	}

	cfg := engineCfgFromQuery(q)
	cfg.Symbol = symbol
	// Optional higher-timeframe alignment: skip entries against the HTF trend.
	if htf := q.Get("htf"); htf != "" && htf != "off" {
		hd, ok := intervals[htf]
		if !ok {
			return fmt.Errorf("unsupported htf %q", htf)
		}
		cfg.HTFAlign, cfg.HTFPeriod = true, hd
	}

	// Window: an explicit from/to date range (plus engine warmup slack before
	// `from`), or the last `bars` candles. Drop a still-forming last candle.
	now := time.Now().UTC()
	end := now
	if to := q.Get("to"); to != "" {
		t, err := time.Parse("2006-01-02", to)
		if err != nil {
			return fmt.Errorf("bad to date %q (want YYYY-MM-DD)", to)
		}
		if e := t.Add(24 * time.Hour); e.Before(now) {
			end = e
		}
	}
	var start time.Time
	warmup := d * time.Duration(cfg.ATRPeriod+8*cfg.PivotN+10)
	// The HTF trend tracker needs many HTF buckets before it can even confirm
	// a swing — without this, ranged runs leave the HTF filter inert.
	if cfg.HTFAlign {
		if hw := cfg.HTFPeriod * time.Duration(8*cfg.HTFPivotN+40); hw > warmup {
			warmup = hw
		}
	}
	if from := q.Get("from"); from != "" {
		t, err := time.Parse("2006-01-02", from)
		if err != nil {
			return fmt.Errorf("bad from date %q (want YYYY-MM-DD)", from)
		}
		start = t.Add(-warmup) // warmup candles so signals exist from day one
	} else {
		start = end.Add(-d * time.Duration(bars+cfg.ATRPeriod+10))
	}
	if !start.Before(end) {
		return fmt.Errorf("from must be before to")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	ks, err := dataset.LoadKlines(ctx, client, dataDir, symbol, interval, start, end, true)
	if err != nil {
		return fmt.Errorf("load klines %s %s: %w", symbol, interval, err)
	}
	for len(ks) > 0 && ks[len(ks)-1].CloseTime.After(end) {
		ks = ks[:len(ks)-1]
	}
	// Keep ranged queries renderable: cap at 20k candles (keep the most recent).
	if len(ks) > 20000 {
		ks = ks[len(ks)-20000:]
	}
	if len(ks) < 50 {
		return fmt.Errorf("only %d closed candles for %s %s in that range", len(ks), symbol, interval)
	}

	resp := replay(ks, cfg)
	resp.Symbol, resp.Interval = symbol, interval
	resp.Config = map[string]any{
		"pivotN": cfg.PivotN, "targetR": cfg.TargetR, "maxStopATR": cfg.MaxStopATR,
		"minStopPct": cfg.MinStopFrac * 100, "signals": cfg.Signals,
		"fvg": cfg.UseFVG, "fvgMid": cfg.FVGMidpoint, "fvgLookback": cfg.FVGLookback,
		"fvgMinATR": cfg.FVGMinATR, "moveStopBOS": cfg.MoveStopOnBOS,
		"breakEven": cfg.BreakEven, "strictChoch": cfg.StrictCHoCH,
		"trendModel": cfg.TrendModel, "swingDev": cfg.SwingDevATR, "luxLen": cfg.LuxLen,
		"htf": q.Get("htf"), "fvgStop": cfg.FVGStop, "fvgStopBuf": cfg.FVGStopBufATR,
		"partialR": cfg.PartialAtR, "ladder": cfg.ScaleOut, "ladderStep": cfg.ScaleStepR,
		"ladderTrail": cfg.ScaleTrailR, "ladderMax": cfg.ScaleMaxR,
		"sessions": cfg.BlackoutSessions, "sessionBuf": cfg.SessionBufMin,
		"rtrail": cfg.RTrail, "rtrailStart": cfg.RTrailStart, "rtrailStep": cfg.RTrailStep,
		"rtrailOff": cfg.RTrailOffset, "bars": len(ks),
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(resp)
}

// replay runs the engine over the klines, accepting every proposal (no
// portfolio gate — this verifies the per-symbol logic in isolation).
func replay(ks []kline.Kline, cfg structure.Config) *runResp {
	eng := structure.New(cfg)
	var events []structure.Event
	eng.SetEventHook(func(ev structure.Event) { events = append(events, ev) })

	var trades []*strategy.Trade
	for _, k := range ks {
		res := eng.Step(k)
		if res.Proposal != nil {
			eng.Resolve(true)
		}
		if res.Closed != nil {
			trades = append(trades, res.Closed)
		}
	}

	resp := &runResp{
		Candles: make([]candleJSON, len(ks)),
		Events:  make([]eventJSON, 0, len(events)),
		Trades:  []tradeJSON{}, // keep JSON as [] (never null) when there are no trades
	}
	for i, k := range ks {
		resp.Candles[i] = candleJSON{T: k.OpenTime.Unix(), O: k.Open, H: k.High, L: k.Low, C: k.Close, V: k.Volume}
	}
	var entries []structure.Event
	for _, ev := range events {
		if ev.Type == "entry" {
			entries = append(entries, ev)
		}
		ej := eventJSON{
			Type: ev.Type, T: ev.Time.Unix(), Side: ev.Side, Setup: ev.Setup,
			Price: ev.Price, Level: ev.Level, Lo: ev.Lo, Hi: ev.Hi,
			Stop: ev.Stop, Target: ev.Target, R: ev.R, Reason: ev.Reason,
		}
		if !ev.Time2.IsZero() {
			ej.T2 = ev.Time2.Unix()
		}
		resp.Events = append(resp.Events, ej)
	}

	// Entries pair 1:1 in order with closed trades; a trailing unmatched entry
	// is the still-open position. Times are re-keyed to the fill bar's open time
	// (entry/exit events carry it) so the frontend can index candles directly.
	exits := make([]eventJSON, 0, len(trades))
	for _, ev := range resp.Events {
		if ev.Type == "exit" {
			exits = append(exits, ev)
		}
	}
	for i, tr := range trades {
		tj := tradeJSON{
			Side: strings.ToLower(tr.Side.String()), Setup: tr.Setup,
			EntryT: tr.EntryTime.Unix(), Entry: tr.EntryPrice,
			InitStop: tr.Stop, Stop: tr.Stop, Target: tr.Target,
			ExitT: tr.ExitTime.Unix(), Exit: tr.ExitPrice,
			Outcome: string(tr.Outcome), R: tr.R,
		}
		if i < len(entries) {
			tj.EntryT = entries[i].Time.Unix()
			tj.InitStop = entries[i].Stop
		}
		if i < len(exits) {
			tj.ExitT = exits[i].T
		}
		resp.Trades = append(resp.Trades, tj)
		resp.NetR += tr.R
	}
	if len(entries) > len(trades) {
		last := entries[len(entries)-1]
		op := eng.OpenPosition()
		o := &openJSON{
			Side: last.Side, Setup: last.Setup, EntryT: last.Time.Unix(),
			Entry: last.Price, Stop: last.Stop, Target: last.Target,
		}
		if op != nil {
			o.Stop = op.Stop // current stop, after any trailing
		}
		resp.Open = o
	}
	return resp
}

// engineCfgFromQuery builds the structure engine config shared by /api/run and
// /api/scan (Symbol and HTFAlign are applied by the callers).
func engineCfgFromQuery(q url.Values) structure.Config {
	return structure.Config{
		PivotN:           qInt(q.Get("pivotN"), 3),
		ATRPeriod:        14,
		MaxStopATR:       qFloat(q.Get("maxStopATR"), 3),
		MinStopFrac:      qFloat(q.Get("minStopPct"), 0.05) / 100,
		TargetR:          qFloat(q.Get("targetR"), 4),
		Signals:          qStr(q.Get("signals"), "both"),
		UseFVG:           qBool(q.Get("fvg"), true),
		FVGLookback:      qInt(q.Get("fvgLookback"), 5),
		FVGMinATR:        qFloat(q.Get("fvgMinATR"), 0),
		FVGMidpoint:      qBool(q.Get("fvgMid"), false),
		MoveStopOnBOS:    qBool(q.Get("moveStopBOS"), false),
		BreakEven:        qBool(q.Get("breakEven"), false),
		StrictCHoCH:      qBool(q.Get("strictChoch"), true),
		TrendModel:       qStr(q.Get("trendModel"), "pivot"),
		LuxLen:           qInt(q.Get("luxLen"), 50),
		SwingDevATR:      qFloat(q.Get("swingDev"), 1),
		PartialAtR:       qFloat(q.Get("partialR"), 0),
		FVGStop:          qBool(q.Get("fvgStop"), false),
		FVGStopBufATR:    qFloat(q.Get("fvgStopBuf"), 0.1),
		ScaleOut:         qBool(q.Get("ladder"), false),
		ScaleStepR:       qFloat(q.Get("ladderStep"), 2),
		ScaleTrailR:      qFloat(q.Get("ladderTrail"), 1),
		ScaleMaxR:        qFloat(q.Get("ladderMax"), 10),
		BlackoutSessions: qStr(q.Get("sessions"), "asia,london,us"),
		SessionBufMin:    qInt(q.Get("sessionBuf"), 30),
		RTrail:           qBool(q.Get("rtrail"), true),
		RTrailStart:      qFloat(q.Get("rtrailStart"), 1),
		RTrailStep:       qFloat(q.Get("rtrailStep"), 1),
		RTrailOffset:     qFloat(q.Get("rtrailOff"), 1),
	}
}

// ---- /api/scan: the screener over a liquidity-filtered universe ----

type scanRowJSON struct {
	Symbol  string  `json:"symbol"`
	Stage   string  `json:"stage"`
	Side    string  `json:"side"`
	Setup   string  `json:"setup"`
	T       int64   `json:"t"`
	Entry   float64 `json:"entry"`
	Stop    float64 `json:"stop"`
	Target  float64 `json:"target"`
	Score   int     `json:"score"`
	Parts   string  `json:"parts"`
	Resting bool    `json:"resting"`
}

func handleScan(w http.ResponseWriter, r *http.Request, client *binance.Client, dataDir string) error {
	q := r.URL.Query()
	interval := qStr(q.Get("interval"), "4h")
	d, ok := intervals[interval]
	if !ok {
		return fmt.Errorf("unsupported interval %q", interval)
	}
	top := qInt(q.Get("top"), 30)
	if top < 5 {
		top = 5
	}
	if top > 100 {
		top = 100
	}
	bars := qInt(q.Get("bars"), 300)
	if bars < 100 {
		bars = 100
	}
	if bars > 1000 {
		bars = 1000
	}
	recent := qInt(q.Get("recent"), 10)

	scfg := screener.Config{
		Interval: interval, D: d, Bars: bars, HTFPivotN: 3,
		W: screener.DefaultWeights(), Engine: engineCfgFromQuery(q),
	}
	// The screener GRADES HTF alignment rather than hard-filtering, so the
	// engine's own HTF filter stays off here.
	htf := qStr(q.Get("htf"), "1d")
	if htf == "off" {
		htf = "1d"
	}
	if hd, ok := intervals[htf]; ok {
		scfg.HTFD = hd
	}

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	syms, err := universe.Fetch(ctx, client, universe.Options{QuoteAsset: "USDT", TopN: top})
	if err != nil {
		return fmt.Errorf("select universe: %w", err)
	}
	names := make([]string, len(syms))
	for i, s := range syms {
		names[i] = s.Symbol
	}
	sc := screener.New(scfg, names)
	var all []screener.Candidate
	sc.Scan(ctx, client, dataDir, func(c screener.Candidate) { all = append(all, c) })

	cutoff := time.Now().UTC().Add(-d * time.Duration(recent))
	seen := map[string]bool{}
	key := func(c screener.Candidate) string {
		return fmt.Sprintf("%s|%s|%d|%.8g", c.Symbol, c.Side, c.Time.Unix(), c.Entry)
	}
	rows := sc.Resting()
	for _, c := range rows {
		seen[key(c)] = true
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
	out := make([]scanRowJSON, len(rows))
	for i, c := range rows {
		out[i] = scanRowJSON{
			Symbol: c.Symbol, Stage: c.Stage, Side: c.Side, Setup: c.Setup, T: c.Time.Unix(),
			Entry: c.Entry, Stop: c.Stop, Target: c.Target, Score: c.Score, Parts: c.Parts, Resting: c.Resting,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"interval": interval, "htf": htf, "candidates": out})
}

func qInt(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

func qFloat(s string, def float64) float64 {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	return def
}

func qBool(s string, def bool) bool {
	switch s {
	case "1", "true", "on":
		return true
	case "0", "false", "off":
		return false
	}
	return def
}

func qStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
