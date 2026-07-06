// Package binance fetches free historical 5m klines from the Binance REST API
// for backtesting, with an on-disk cache so repeated backtests don't re-hit the
// network. Public market data needs no API key.
package binance

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

// defaultBaseURL is Binance's public market-data REST mirror. It serves the
// same /api/v3 endpoints as api.binance.com but is intended for public data:
// no API key, and it avoids the geo-DNS restrictions that can make
// api.binance.com unresolvable from some networks. Override with BINANCE_BASE_URL.
const defaultBaseURL = "https://data-api.binance.vision"

// maxLimit is Binance's per-request kline cap.
const maxLimit = 1000

// Client fetches klines. The zero value is not usable; use NewClient.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Binance REST client with sane timeouts. The base URL is
// the public data mirror, overridable via the BINANCE_BASE_URL env var.
func NewClient() *Client {
	base := defaultBaseURL
	if v := os.Getenv("BINANCE_BASE_URL"); v != "" {
		base = v
	}
	return &Client{
		baseURL: base,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Klines fetches all 5m (or other interval) klines in [start, end), paginating
// across Binance's 1000-per-request limit. Returned klines are ordered by open
// time and exclude the still-open final candle.
func (c *Client) Klines(ctx context.Context, symbol, interval string, start, end time.Time) ([]kline.Kline, error) {
	var out []kline.Kline
	cursor := start.UnixMilli()
	endMs := end.UnixMilli()

	for cursor < endMs {
		batch, err := c.klinesPage(ctx, symbol, interval, cursor, endMs)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, k := range batch {
			if k.OpenTime.UnixMilli() >= endMs {
				return out, nil
			}
			out = append(out, k)
		}
		last := batch[len(batch)-1].OpenTime.UnixMilli()
		if last < cursor { // safety against a non-advancing cursor
			break
		}
		cursor = last + 1
		if len(batch) < maxLimit {
			break // last page
		}
	}
	return out, nil
}

func (c *Client) klinesPage(ctx context.Context, symbol, interval string, startMs, endMs int64) ([]kline.Kline, error) {
	q := url.Values{}
	q.Set("symbol", symbol)
	q.Set("interval", interval)
	q.Set("startTime", strconv.FormatInt(startMs, 10))
	q.Set("endTime", strconv.FormatInt(endMs, 10))
	q.Set("limit", strconv.Itoa(maxLimit))
	u := c.baseURL + "/api/v3/klines?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("klines request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("klines status %d for %s", resp.StatusCode, symbol)
	}
	return ParseKlines(resp.Body)
}

// ParseKlines decodes the Binance REST klines array form into Klines.
func ParseKlines(r io.Reader) ([]kline.Kline, error) {
	var raw [][]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode klines: %w", err)
	}
	out := make([]kline.Kline, 0, len(raw))
	for i, row := range raw {
		if len(row) < 7 {
			return nil, fmt.Errorf("kline row %d: expected >=7 fields, got %d", i, len(row))
		}
		openMs, err := jsonInt(row[0])
		if err != nil {
			return nil, fmt.Errorf("kline row %d openTime: %w", i, err)
		}
		closeMs, err := jsonInt(row[6])
		if err != nil {
			return nil, fmt.Errorf("kline row %d closeTime: %w", i, err)
		}
		o, _ := jsonFloatStr(row[1])
		h, _ := jsonFloatStr(row[2])
		l, _ := jsonFloatStr(row[3])
		cl, _ := jsonFloatStr(row[4])
		v, _ := jsonFloatStr(row[5])
		out = append(out, kline.Kline{
			OpenTime:  time.UnixMilli(openMs).UTC(),
			Open:      o,
			High:      h,
			Low:       l,
			Close:     cl,
			Volume:    v,
			CloseTime: time.UnixMilli(closeMs).UTC(),
		})
	}
	return out, nil
}

// jsonInt reads a JSON number (Binance encodes times as bare integers).
func jsonInt(m json.RawMessage) (int64, error) {
	return strconv.ParseInt(string(m), 10, 64)
}

// jsonFloatStr reads a JSON string-encoded decimal (Binance encodes prices as
// quoted strings, e.g. "1.2345").
func jsonFloatStr(m json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(m, &s); err != nil {
		return 0, err
	}
	return strconv.ParseFloat(s, 64)
}

// --- on-disk cache (NDJSON of Klines) ---

// CachePath returns the cache file path for a symbol/interval under dataDir.
func CachePath(dataDir, symbol, interval string) string {
	return filepath.Join(dataDir, "klines", fmt.Sprintf("%s-%s.ndjson", symbol, interval))
}

// SaveKlines writes klines as NDJSON, creating parent dirs.
func SaveKlines(path string, ks []kline.Kline) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, k := range ks {
		if err := enc.Encode(k); err != nil {
			return err
		}
	}
	return w.Flush()
}

// LoadKlines reads NDJSON klines written by SaveKlines.
func LoadKlines(path string) ([]kline.Kline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var ks []kline.Kline
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var k kline.Kline
		if err := json.Unmarshal(sc.Bytes(), &k); err != nil {
			return nil, err
		}
		ks = append(ks, k)
	}
	return ks, sc.Err()
}
