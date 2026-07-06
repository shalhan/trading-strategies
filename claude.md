# 4H opening-range failed-break bot (multi-symbol)

A trading bot that scans a universe of liquid Binance USDT pairs. For each
symbol it defines a daily range from the first 4-hour window of the New York
day, then trades **failed breaks** of that range (fade the fakeout) on the
5-minute timeframe. One independent state machine per symbol. Go. Same-day only.

v1 is pure price action and backtestable on free historical klines. Order flow
is a v2 filter, not part of v1.

## Build order (do not reorder)

1. **Backtester first.** Replay historical 5m klines and run the strategy
   deterministically. Every rule must be measurable before any live code.
2. **Strategy engine**, symbol-agnostic, validated in the backtester. Debug the
   per-symbol logic on ONE symbol's trade log first, then run the full universe.
3. **Portfolio risk layer** (see below) — required before multi-symbol live.
4. **Tune parameters** on backtest results. No universal values.
5. **Paper trade**, then live, last.
6. **In parallel (v2 only):** an aggTrade recorder for order-flow data. v1 does
   not depend on it.

## Universe — what to scan

- A liquidity-filtered set, not every coin. Default: top N USDT pairs by 30-day
  volume (start N ~ 100, tunable).
- Exclude leveraged tokens (UP/DOWN/BULL/BEAR), stable-to-stable pairs, and
  pairs with insufficient history.
- Thin coins produce fake wicks and bad fills — they generate false signals.
  The liquidity filter is a quality filter.

## Levels — the daily range (per symbol)

- Take the **first 4-hour window of the New York day** (assumed 00:00–04:00 ET;
  CONFIRM this is NY midnight, not the 09:30 session). High and low of that
  window are the range.
- Compute as `max(high)` / `min(low)` of the 5m candles in the window — do NOT
  use Binance's native 4H kline (UTC-aligned, won't match the NY window in
  winter). Anchor to `America/New_York` (DST-aware).
- Usable only **after the window closes** (from 04:00 ET). Resets each NY day.

## Setup — failed-break reversal (both directions, per symbol)

A break is confirmed by a 5m **close** beyond the level (not just a wick).

- **Short:** a 5m candle closes **above** the range high, then a later 5m
  candle closes **back below** the high. Enter short at the reentry candle's
  close.
- **Long:** a 5m candle closes **below** the range low, then a later 5m candle
  closes **back above** the low. Enter long at the reentry candle's close.

While broken, track the **false-break extreme**: the highest high above the
level (shorts) or lowest low below it (longs), updated each candle until reentry.

## Stop and target

- **Stop = the false-break extreme** plus a small buffer (a few ticks). The true
  invalidation: if price returns past the extreme, the thesis is dead.
- **Max-stop-distance filter:** if `abs(entry - extreme)` exceeds
  `MAX_STOP_ATR` × (5m ATR), **skip the trade**. ATR-based, not fixed %, so it
  behaves consistently across coins of different volatility. Do NOT tighten the
  stop inside the extreme to compensate — skip instead.
- **Target = 2R** (2 × entry-to-stop distance).

## Per-symbol rules

- **Same NY day only.** Force-close any open position at NY midnight (00:00 ET).
- **Both directions** across the day, but **one position per symbol at a time**.
  Re-arm after each trade closes (stop, target, or EOD).
- **Max attempts per side per day:** default 2, tunable.

## Portfolio risk (the critical multi-symbol layer)

Alts are highly correlated — many will fail-break together in a market move, so
simultaneous signals are NOT diversified; they are one correlated bet.

- **Max concurrent positions** cap (e.g. start at 5).
- **Max total open risk** cap across all positions (e.g. 5% of capital), no
  matter how many signals fire.
- **Per-trade risk:** `risk_per_trade (e.g. 1%) / stop_distance`.
- When more signals fire than there is room/risk budget for, **rank and take
  the best** (e.g. tightest stop distance) and skip the rest.

## State machine (per symbol)

`WAIT_RANGE → WATCHING → BROKEN → IN_POSITION → FLAT → (re-arm)`

- WAIT_RANGE: before the 4H window closes; no levels yet.
- WATCHING: range set; watch for a 5m close beyond high or low.
- BROKEN: track the false-break extreme; watch for a 5m close back inside →
  apply max-stop-distance filter AND portfolio risk check before entering.
- IN_POSITION: entered at reentry close; manage stop and 2R target.
- FLAT: closed; re-arm if under attempts cap and same NY day, else WAIT_RANGE
  next NY day.

## Data

- **v1:** 5m OHLCV klines only. Historical via Binance REST klines endpoint
  (free) per symbol for backtesting; live via combined kline websocket streams.
  Mind Binance's per-connection stream limits — multiplex across connections for
  a large universe. No order book, no aggTrades needed.
- **v2:** aggTrade stream for order flow.

## v2 — order flow as a filter (later)

Once v1 has a measured baseline, only take a reentry if there was **absorption /
delta exhaustion at the false-break extreme** (aggression into the break that
failed to push price). Targets the main failure mode: a "failed break" that was
really a breakout pausing. Compare to the v1 baseline and keep only if it
measurably helps. The aggTrade `m` field gives aggressor side directly.

## Known constraints and risks

- Mean-reversion: loses when a break is real and continues. The stop, the
  max-distance filter, and the portfolio caps are the protections.
- Correlation risk across symbols is the main multi-symbol danger.
- All thresholds MUST be tuned on the backtest. Validate in backtest, then
  paper, before risking capital. Profitability is unproven; not financial advice.

## Tech and conventions

- Go. Explicit per-symbol state machine, not ad-hoc conditionals.
- One engine instance per symbol; a portfolio manager mediates risk and entries.
- All time/session logic via `America/New_York` (DST-aware), never a hardcoded
  UTC offset.
- Confirmation-close entries (deterministic) so backtest and live agree.
- Secrets via env vars only; user supplies own keys at runtime.
- Every parameter configurable; every trade logged for analysis.

## Parameters to tune

- `UNIVERSE_SIZE` — top N USDT pairs by volume; default 100.
- `MAX_STOP_ATR` — ATR multiple; skip setups with a wider stop.
- `MAX_ATTEMPTS_PER_SIDE` — default 2.
- `MAX_CONCURRENT_POSITIONS` — default 5.
- `MAX_TOTAL_RISK` — default 5% of capital.
- `RISK_PER_TRADE` — default 1% of capital.