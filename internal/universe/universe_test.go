package universe

import (
	"testing"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
)

func TestIsLeveraged(t *testing.T) {
	cases := map[string]bool{
		"BTCUP":   true,
		"BTCDOWN": true,
		"ETHBULL": true,
		"ETHBEAR": true,
		"JUP":     false, // legitimate short ticker, not "J"+UP leveraged
		"BTC":     false,
		"ETHFI":   false,
		"DOWN":    false, // no underlying prefix
		"PEPE":    false,
	}
	for base, want := range cases {
		if got := IsLeveraged(base); got != want {
			t.Errorf("IsLeveraged(%q)=%v, want %v", base, got, want)
		}
	}
}

func TestSelectFiltersAndRanks(t *testing.T) {
	meta := []binance.SymbolMeta{
		{Symbol: "ETHFIUSDT", BaseAsset: "ETHFI", QuoteAsset: "USDT", Status: "TRADING", TickSize: 0.001},
		{Symbol: "BTCUSDT", BaseAsset: "BTC", QuoteAsset: "USDT", Status: "TRADING", TickSize: 0.01},
		{Symbol: "ETHUSDT", BaseAsset: "ETH", QuoteAsset: "USDT", Status: "TRADING", TickSize: 0.01},
		{Symbol: "USDCUSDT", BaseAsset: "USDC", QuoteAsset: "USDT", Status: "TRADING", TickSize: 0.0001}, // stable: excluded
		{Symbol: "BTCUPUSDT", BaseAsset: "BTCUP", QuoteAsset: "USDT", Status: "TRADING", TickSize: 0.01}, // leveraged: excluded
		{Symbol: "ETHBTC", BaseAsset: "ETH", QuoteAsset: "BTC", Status: "TRADING", TickSize: 0.00001},    // non-USDT: excluded
		{Symbol: "DEADUSDT", BaseAsset: "DEAD", QuoteAsset: "USDT", Status: "BREAK", TickSize: 0.001},     // not trading: excluded
		{Symbol: "NOTICKUSDT", BaseAsset: "NOTICK", QuoteAsset: "USDT", Status: "TRADING", TickSize: 0},   // no tick: excluded
	}
	tickers := []binance.Ticker24h{
		{Symbol: "ETHFIUSDT", QuoteVolume: 50e6},
		{Symbol: "BTCUSDT", QuoteVolume: 900e6},
		{Symbol: "ETHUSDT", QuoteVolume: 400e6},
		{Symbol: "USDCUSDT", QuoteVolume: 999e6},
	}

	got := Select(meta, tickers, Options{QuoteAsset: "USDT", TopN: 2})
	if len(got) != 2 {
		t.Fatalf("got %d symbols, want 2 (TopN)", len(got))
	}
	// Ranked by volume: BTC (900) then ETH (400); ETHFI (50) drops off TopN=2.
	if got[0].Symbol != "BTCUSDT" || got[1].Symbol != "ETHUSDT" {
		t.Errorf("ranking wrong: %s, %s; want BTCUSDT, ETHUSDT", got[0].Symbol, got[1].Symbol)
	}
	if got[0].TickSize != 0.01 {
		t.Errorf("tick size not propagated: %v", got[0].TickSize)
	}
}
