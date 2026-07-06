// Package dataset loads historical klines for backtesting, transparently
// caching them on disk so repeated runs (e.g. a parameter sweep) need no
// network after the first fetch.
package dataset

import (
	"context"
	"time"

	"github.com/shalhan/orderflow-trading-app/internal/binance"
	"github.com/shalhan/orderflow-trading-app/internal/kline"
)

// LoadKlines returns cached klines for the symbol if present and covering the
// requested window (unless refresh), otherwise fetches them from Binance and
// caches them. The coverage check means widening -days re-fetches rather than
// silently returning a shorter cached range. It is silent; callers handle
// progress reporting.
func LoadKlines(ctx context.Context, c *binance.Client, dataDir, symbol, interval string, start, end time.Time, refresh bool) ([]kline.Kline, error) {
	cache := binance.CachePath(dataDir, symbol, interval)
	if !refresh {
		if ks, err := binance.LoadKlines(cache); err == nil && len(ks) > 0 && covers(ks, start) {
			return trim(ks, start, end), nil
		}
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ks, err := c.Klines(fctx, symbol, interval, start, end)
	if err != nil {
		return nil, err
	}
	if err := binance.SaveKlines(cache, ks); err != nil {
		return nil, err
	}
	return ks, nil
}

// covers reports whether the cached series reaches back to (near) the requested
// start. A 2-day tolerance absorbs symbols that simply did not trade that early.
func covers(ks []kline.Kline, start time.Time) bool {
	return !ks[0].OpenTime.After(start.Add(48 * time.Hour))
}

// trim returns the sub-slice of klines whose open time is in [start, end), so a
// larger cache yields exactly the requested window and all symbols stay aligned.
func trim(ks []kline.Kline, start, end time.Time) []kline.Kline {
	lo := 0
	for lo < len(ks) && ks[lo].OpenTime.Before(start) {
		lo++
	}
	hi := len(ks)
	for hi > lo && !ks[hi-1].OpenTime.Before(end) {
		hi--
	}
	return ks[lo:hi]
}
