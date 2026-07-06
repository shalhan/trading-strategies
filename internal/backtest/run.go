package backtest

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
	"github.com/shalhan/orderflow-trading-app/internal/strategy"
)

// RunSymbol replays klines through a single-symbol engine and returns every
// completed trade in order. Klines must be sorted by open time. With no
// portfolio constraint, every proposed entry is accepted immediately.
func RunSymbol(cfg strategy.Config, ks []kline.Kline) []*strategy.Trade {
	e := strategy.New(cfg)
	var trades []*strategy.Trade
	for _, k := range ks {
		res := e.Step(k)
		if res.Proposal != nil {
			e.Resolve(true)
		}
		if res.Closed != nil {
			trades = append(trades, res.Closed)
		}
	}
	return trades
}

// WriteTradeLogCSV writes the trade log to path (parent dirs created). Every
// trade is logged so results can be analysed offline (CLAUDE.md).
func WriteTradeLogCSV(path string, trades []*strategy.Trade) error {
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

	header := []string{
		"symbol", "side", "setup", "entry_time", "entry", "trigger", "stop", "target", "stop_dist",
		"exit_time", "exit", "outcome", "r", "maker", "bep_armed",
	}
	if err := w.Write(header); err != nil {
		return err
	}
	for _, t := range trades {
		rec := []string{
			t.Symbol,
			t.Side.String(),
			t.Setup,
			t.EntryTime.UTC().Format("2006-01-02T15:04:05Z"),
			ftoa(t.EntryPrice),
			ftoa(t.Trigger),
			ftoa(t.Stop),
			ftoa(t.Target),
			ftoa(t.StopDist),
			t.ExitTime.UTC().Format("2006-01-02T15:04:05Z"),
			ftoa(t.ExitPrice),
			string(t.Outcome),
			strconv.FormatFloat(t.R, 'f', 4, 64),
			strconv.FormatBool(t.MakerEntry),
			strconv.FormatBool(t.BEPArmed),
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	return w.Error()
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
