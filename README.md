# 4H opening-range failed-break bot (multi-symbol)

Go trading bot that fades failed breaks of each symbol's first-4-hour New-York
range on the 5m timeframe. See [`claude.md`](./claude.md) for the full spec.

**v1 is pure price action, backtested on free historical Binance klines.** Order
flow is a deferred v2 filter.

## Status

Build order per the spec is _backtester first_, then strategy, then the
portfolio risk layer, then live.

- [x] **Backtester** — deterministic 5m kline replay, trade log, stats.
- [x] **Strategy engine** (single-symbol) — the `WAIT_RANGE → WATCHING → BROKEN
      → PENDING_ENTRY → IN_POSITION → FLAT` state machine, ATR-based stop
      filter, per-side attempt cap, NY-midnight force-close. Two-phase entry
      (propose → resolve) so the portfolio can rank before committing.
- [x] **Universe selection** — top-N liquid USDT pairs (24h volume), excluding
      leveraged tokens and stablecoins.
- [x] **Portfolio risk layer** — concurrent-position and total-risk caps;
      when more signals fire than fit, rank by tightest relative stop and skip
      the rest. Replays all symbols on one shared 5m timeline.
- [x] **Parameter tuning** — grid sweep with a train/test split so settings are
      chosen for out-of-sample robustness, not curve fit.
- [ ] **Live** — websocket klines, paper then real.

## Layout

```
cmd/backtest        CLI: run one symbol, report + trade log
cmd/scan            CLI: select universe, run multi-symbol portfolio backtest
cmd/tune            CLI: grid-sweep parameters with a train/test split
cmd/trendscan       CLI: multi-timeframe trendline screener (touch / breakout-retest alerts)
internal/kline      5m OHLCV candle + New-York-day helpers
internal/trendline  pivot-pair trendline detection + touch/retest classification
internal/indicator  streaming Wilder ATR (sizes the stop filter)
internal/strategy   per-symbol failed-break state machine (the core)
internal/universe   liquidity-filtered symbol selection
internal/portfolio  cross-symbol risk caps + rank-and-take-best
internal/binance    Binance REST klines/market fetcher + on-disk cache
internal/dataset    cache-or-fetch kline loader (window-aware)
internal/backtest   single + portfolio replay runners, stats, CSV trade log
```

## Run a backtest

Public market data needs no API key.

```sh
go build -o bin/backtest ./cmd/backtest
./bin/backtest -symbol ETHFIUSDT -days 60
```

First run fetches and caches klines under `data/klines/`; reruns reuse the cache
(`-refresh` to re-fetch). The trade log is written to `out/`.

Key flags (every parameter is tunable; **none are universal — tune on the
backtest**):

| flag | default | meaning |
|------|---------|---------|
| `-max-stop-atr` | 3.0 | skip setups whose stop distance exceeds this × 5m ATR (failed-break stops run ~2–19× ATR, so <2 takes nothing) |
| `-max-attempts` | 2 | entries per side per NY day |
| `-stop-buffer-ticks` | 2 | buffer beyond the false-break extreme |
| `-tick-size` | 0.0001 | symbol price tick |
| `-atr-period` | 14 | Wilder ATR period |
| `-capital` / `-risk` | 10000 / 0.01 | account sim: risk per trade |

## Run a multi-symbol portfolio backtest

```sh
go build -o bin/scan ./cmd/scan
./bin/scan -top 20 -days 30
```

Selects the top-N liquid USDT pairs, replays them all on one 5m timeline, and
applies the portfolio caps across them. Portfolio flags:

| flag | default | meaning |
|------|---------|---------|
| `-top` | 20 | universe size (top-N by 24h volume) |
| `-max-concurrent` | 5 | max simultaneous open positions |
| `-max-total-risk` | 0.05 | max total open risk as a fraction of capital |
| `-risk` | 0.01 | risk per trade (position sizing) |

When more signals fire on a bar than fit the caps, the tightest-relative-stop
signals win and the rest are skipped — correlated alts firing together count as
one bet, not diversification.

## Tune parameters

```sh
go build -o bin/tune ./cmd/tune
./bin/tune -top 20 -days 45
```

Loads the data once, then sweeps `MAX_STOP_ATR × MAX_ATTEMPTS × MAX_CONCURRENT`
over a `-train` / test split (default 70/30). Each combo is scored in-sample and
on the held-out test set, ranked by the in-sample objective (`-objective totalr
| expectancy | pf`). **Pick a combo that is positive and stable on _both_
splits** — one that is strong in-sample but weak out-of-sample is curve-fit, not
an edge. Full grid is written to `out/tune-results.csv`.

Grid flags: `-stop-atr "2,2.5,3,4,5"`, `-attempts "1,2,3"`, `-concurrent "3,5,8"`.

## Trendline screener (breakout-and-retest)

```sh
go build -o bin/trendscan ./cmd/trendscan
./bin/trendscan -top 50            # one-shot snapshot
./bin/trendscan -top 50 -watch -notify   # stream new alerts each 1m close (+ macOS banner)
```

Builds the best trendline per side (support/resistance) on each timeframe
(default `1d,4h,1h,30m,5m,1m`) — the line through two swing pivots that touches
the most candles without ever being broken by a close — then reports every coin
whose current price is at one of those lines. `TOUCH` = intact line (watch for
the break); `RETEST` = line broken within the last `-recent-break` candles and
price back at it — the breakout-and-retest entry.

| flag | default | meaning |
|------|---------|---------|
| `-tfs` | `1d,4h,1h,30m,5m,1m` | timeframes to build lines on |
| `-min-touches` | 3 | touches a line needs to qualify |
| `-touch-atr` / `-break-atr` | 0.25 / 0.25 | touch tolerance / break confirmation, in that timeframe's ATR |
| `-recent-break` | 12 | candles a break may be old and still count as a retest |
| `-pivot-n` / `-min-span` | 3 / 10 | pivot strength / min bars between line anchors |

## Test

```sh
go test ./...
```

Conventions: all session logic is anchored to `America/New_York` (DST-aware);
entries are confirmation-close so backtest and live agree; secrets via env vars
only.
