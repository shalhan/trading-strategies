package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// SymbolMeta is the exchange metadata needed to trade a symbol.
type SymbolMeta struct {
	Symbol     string
	BaseAsset  string
	QuoteAsset string
	Status     string  // "TRADING" when live
	TickSize   float64 // price increment from the PRICE_FILTER
}

// ExchangeInfo fetches per-symbol metadata (status, assets, tick size).
func (c *Client) ExchangeInfo(ctx context.Context) ([]SymbolMeta, error) {
	var body struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Filters    []struct {
				FilterType string `json:"filterType"`
				TickSize   string `json:"tickSize"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := c.getJSON(ctx, "/api/v3/exchangeInfo", &body); err != nil {
		return nil, err
	}
	out := make([]SymbolMeta, 0, len(body.Symbols))
	for _, s := range body.Symbols {
		var tick float64
		for _, f := range s.Filters {
			if f.FilterType == "PRICE_FILTER" {
				tick, _ = strconv.ParseFloat(f.TickSize, 64)
				break
			}
		}
		out = append(out, SymbolMeta{
			Symbol: s.Symbol, BaseAsset: s.BaseAsset, QuoteAsset: s.QuoteAsset,
			Status: s.Status, TickSize: tick,
		})
	}
	return out, nil
}

// Ticker24h is the rolling 24h stats subset used as a liquidity proxy.
type Ticker24h struct {
	Symbol      string
	QuoteVolume float64 // 24h volume in the quote asset (USDT) — the liquidity metric
}

// Tickers24h fetches 24h rolling stats for every symbol in one call.
func (c *Client) Tickers24h(ctx context.Context) ([]Ticker24h, error) {
	var raw []struct {
		Symbol      string `json:"symbol"`
		QuoteVolume string `json:"quoteVolume"`
	}
	if err := c.getJSON(ctx, "/api/v3/ticker/24hr", &raw); err != nil {
		return nil, err
	}
	out := make([]Ticker24h, 0, len(raw))
	for _, t := range raw {
		qv, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		out = append(out, Ticker24h{Symbol: t.Symbol, QuoteVolume: qv})
	}
	return out, nil
}

// getJSON performs a GET against the base URL and decodes the JSON body.
func (c *Client) getJSON(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
