// Package universe selects the liquidity-filtered set of symbols to scan. Per
// CLAUDE.md the liquidity filter is a quality filter: thin coins throw fake
// wicks and bad fills that generate false failed-break signals.
package universe

import (
	"context"
	"sort"
	"strings"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
)

// Symbol is a selected, tradeable symbol with the metadata the strategy needs.
type Symbol struct {
	Symbol      string
	BaseAsset   string
	TickSize    float64
	QuoteVolume float64 // 24h quote volume — the liquidity ranking metric
}

// Options tunes selection.
type Options struct {
	QuoteAsset string // e.g. "USDT"
	TopN       int    // keep the N most liquid (UNIVERSE_SIZE; default 100)
}

func (o *Options) withDefaults() {
	if o.QuoteAsset == "" {
		o.QuoteAsset = "USDT"
	}
	if o.TopN <= 0 {
		o.TopN = 100
	}
}

// stablecoins are excluded as base assets: a stable/USDT pair has no range to
// trade.
var stablecoins = map[string]bool{
	"USDC": true, "BUSD": true, "TUSD": true, "FDUSD": true, "DAI": true,
	"USDP": true, "USDD": true, "GUSD": true, "PAX": true, "SUSD": true,
	"USTC": true, "EUR": true, "EURI": true, "AEUR": true, "PYUSD": true,
	"USD1": true, "USDE": true, "BFUSD": true, "XUSD": true, "EURT": true,
}

// Select joins exchange metadata with 24h volume and returns the top-N liquid,
// tradeable, non-leveraged, non-stable QuoteAsset pairs, most liquid first.
// Pure and deterministic for testability.
func Select(meta []binance.SymbolMeta, tickers []binance.Ticker24h, opts Options) []Symbol {
	opts.withDefaults()

	vol := make(map[string]float64, len(tickers))
	for _, t := range tickers {
		vol[t.Symbol] = t.QuoteVolume
	}

	var out []Symbol
	for _, m := range meta {
		if m.Status != "TRADING" || m.QuoteAsset != opts.QuoteAsset {
			continue
		}
		if IsLeveraged(m.BaseAsset) || stablecoins[m.BaseAsset] {
			continue
		}
		if m.TickSize <= 0 {
			continue // need a tick size to size the stop buffer
		}
		out = append(out, Symbol{
			Symbol: m.Symbol, BaseAsset: m.BaseAsset,
			TickSize: m.TickSize, QuoteVolume: vol[m.Symbol],
		})
	}

	// Most liquid first; tie-break by symbol for determinism.
	sort.Slice(out, func(i, j int) bool {
		if out[i].QuoteVolume != out[j].QuoteVolume {
			return out[i].QuoteVolume > out[j].QuoteVolume
		}
		return out[i].Symbol < out[j].Symbol
	})
	if len(out) > opts.TopN {
		out = out[:opts.TopN]
	}
	return out
}

// IsLeveraged reports whether a base asset is a Binance leveraged token
// (…UP/…DOWN/…BULL/…BEAR). The prefix-length guards avoid false positives on
// legitimate short tickers such as JUP ("J"+"UP").
func IsLeveraged(base string) bool {
	for _, suf := range []string{"UP", "DOWN"} {
		if strings.HasSuffix(base, suf) && len(base) > len(suf)+2 {
			return true
		}
	}
	for _, suf := range []string{"BULL", "BEAR"} {
		if strings.HasSuffix(base, suf) && len(base) > len(suf) {
			return true
		}
	}
	return false
}

// Fetch retrieves exchange info and 24h tickers from Binance and applies Select.
func Fetch(ctx context.Context, c *binance.Client, opts Options) ([]Symbol, error) {
	meta, err := c.ExchangeInfo(ctx)
	if err != nil {
		return nil, err
	}
	tickers, err := c.Tickers24h(ctx)
	if err != nil {
		return nil, err
	}
	return Select(meta, tickers, opts), nil
}
