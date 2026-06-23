# 🐋 Polytracker

**Real-time whale trade detection for [Polymarket](https://polymarket.com) prediction markets.**

Polytracker is a zero-dependency Go service that polls Polymarket's public APIs, detects trades whose USD value exceeds a configurable percentage of a market's open interest, and outputs enriched JSON alerts with order book depth and top-holder context.

---

## Quick Start

```bash
# Requires Go 1.22+
git clone https://github.com/nesto/polytracker.git
cd polytracker
go build -o polytracker .

# Show help instructions
./polytracker help

# Track all active markets for whale trades (default mode)
./polytracker track

# Track trade history and new trades for a specific wallet
./polytracker track --wallet=0xd81fbc5c53593e4e2923a641ff2bc7e2d9866b75

# Summarize a wallet's realized P&L and win rate across resolved positions
./polytracker wallet-history 0xd81fbc5c53593e4e2923a641ff2bc7e2d9866b75
```

When tracking a specific wallet, the tool retrieves and paginates historical trades 10 at a time (sorted latest to oldest). Pressing `[Enter]` transitions to real-time tracking, polling every `poll_interval`. Log files are saved under `logs/` with names following `session_command_flag_DATE_TIME.log`.

### `wallet-history <wallet>`

Prints a one-shot summary of a wallet's **realized P&L** and **win rate by entry-price bucket** across all of its *resolved* positions:

```
══════════════════════════════════════════════════════════════
 Wallet history: 0xd81f...
══════════════════════════════════════════════════════════════
 Positions scanned:     76
 Resolved positions:    4
 Total realized P&L:    $-84.04
 Overall win rate:      0.0% (0/4)
──────────────────────────────────────────────────────────────
 Win rate by entry price (win = position P&L > 0)
──────────────────────────────────────────────────────────────
 Price          N     Win%            P&L
 0.0-0.1        2       0%       $-40.18
 0.3-0.4        1       0%       $-33.30
 0.4-0.5        1       0%       $-10.56
══════════════════════════════════════════════════════════════
```

- **Resolved** = the market reports `closed=true` (CLOB). Markets that are past their listed end date but still accepting orders are *not* counted — Polymarket has not settled them.
- **Realized P&L per position** = `realizedPnl + cashPnl` (profit locked in from earlier sells, plus the settled value of shares held to resolution). The total is the sum across resolved positions.
- **Win** = a resolved position with positive total P&L. Buckets group positions by entry price (`avgPrice`) into deciles, so you can see whether buying cheap longshots or expensive favorites tends to pay off for this wallet.
- All positions are pulled via `/positions?sizeThreshold=0` (so sold/settled positions aren't hidden); resolution is checked once per unique market, concurrently.

---

## Configuration

All settings live in **`settings.json`** alongside the binary. Edit this file to tune the detector:

```json
{
  "alert_threshold": 0.05,
  "poll_interval": "60s",
  "market_refresh_interval": "5m",
  "max_concurrency": 10,
  "min_open_interest": 10000,
  "max_open_interest": 0,
  "max_markets_per_cycle": 500,
  "min_trade_usd": 0,
  "max_signal_price": 0,
  "min_time_to_resolution": "0s",
  "score_weights": { "size": 0.40, "room": 0.15, "time": 0.15, "action": 0.30 },
  "score_ref_ratio": 0.25,
  "score_ref_days": 30,
  "min_score": 0,
  "gamma_base_url": "https://gamma-api.polymarket.com",
  "data_base_url": "https://data-api.polymarket.com",
  "clob_base_url": "https://clob.polymarket.com",
  "rate_limit_rps": 10,
  "log_level": "warn"
}
```

### Settings Reference

| Field | Default | Description |
|-------|---------|-------------|
| `alert_threshold` | `0.05` | Trade/OI ratio to trigger alert (0.05 = 5%) |
| `poll_interval` | `"60s"` | How often to check each market for new trades |
| `market_refresh_interval` | `"5m"` | How often to rebuild the active market list |
| `max_concurrency` | `10` | Max parallel in-flight API calls |
| `min_open_interest` | `10000` | Minimum open interest (USD) — markets below this are ignored |
| `max_open_interest` | `0` | Maximum open interest (USD) — `0` means no upper limit |
| `max_markets_per_cycle` | `500` | Cap on total markets to monitor |
| `min_trade_usd` | `0` | Signal filter: drop flagged trades below this absolute USD value. `0` disables (alerts still annotated). |
| `max_signal_price` | `0` | Signal filter: drop flagged trades executed above this price (0–1). Near-1.0 = little room to resolution. `0` disables. |
| `min_time_to_resolution` | `"0s"` | Signal filter: drop flagged trades in markets resolving sooner than this. `"0s"` disables. |
| `score_weights` | `{size:0.40, room:0.15, time:0.15, action:0.30}` | Relative weight of each sub-signal in the composite `signalScore`. Auto-normalized — need not sum to 1. |
| `score_ref_ratio` | `0.25` | Trade/OI ratio at which the size sub-score saturates to 1.0 (25% of OI = max). |
| `score_ref_days` | `30` | Days-to-resolution at which the time sub-score saturates to 1.0. |
| `min_score` | `0` | Signal filter: drop flagged trades whose composite `signalScore` (0–100) is below this. `0` disables (alerts still annotated). |
| `gamma_base_url` | `"https://gamma-api.polymarket.com"` | Gamma API base URL |
| `data_base_url` | `"https://data-api.polymarket.com"` | Data API base URL |
| `clob_base_url` | `"https://clob.polymarket.com"` | CLOB API base URL |
| `rate_limit_rps` | `10` | Global cap on API requests per second. Polymarket's Data API rate-limits bursts, so `10` is the tested-stable default. Raising it speeds the startup OI sweep (one `/oi` call per market) but risks `429`s under the poll fan-out; `429`s are retried honoring the server's `Retry-After`. |
| `log_level` | `"warn"` | Operational-log verbosity on **stderr**: `debug`, `info`, `warn`, `error`. Default `warn` keeps the terminal quiet so the readable whale summaries on **stdout** stand out. Set `info` to watch the polling lifecycle. |

Duration fields accept Go duration strings: `"30s"`, `"2m"`, `"1h30m"`, etc.

### Environment Variable Overrides

For containerized deployments, every setting can be overridden via environment variables. Env vars take precedence over `settings.json`:

| Env Variable | Overrides |
|--------------|-----------|
| `PT_ALERT_THRESHOLD` | `alert_threshold` |
| `PT_POLL_INTERVAL` | `poll_interval` |
| `PT_MARKET_REFRESH` | `market_refresh_interval` |
| `PT_MAX_CONCURRENCY` | `max_concurrency` |
| `PT_MIN_OI` | `min_open_interest` |
| `PT_MAX_OI` | `max_open_interest` |
| `PT_MAX_MARKETS` | `max_markets_per_cycle` |
| `PT_MIN_TRADE_USD` | `min_trade_usd` |
| `PT_MAX_SIGNAL_PRICE` | `max_signal_price` |
| `PT_MIN_TIME_TO_RESOLUTION` | `min_time_to_resolution` |
| `PT_SCORE_WEIGHT_SIZE` | `score_weights.size` |
| `PT_SCORE_WEIGHT_ROOM` | `score_weights.room` |
| `PT_SCORE_WEIGHT_TIME` | `score_weights.time` |
| `PT_SCORE_WEIGHT_ACTION` | `score_weights.action` |
| `PT_SCORE_REF_RATIO` | `score_ref_ratio` |
| `PT_SCORE_REF_DAYS` | `score_ref_days` |
| `PT_MIN_SCORE` | `min_score` |
| `PT_GAMMA_URL` | `gamma_base_url` |
| `PT_DATA_URL` | `data_base_url` |
| `PT_CLOB_URL` | `clob_base_url` |
| `PT_RATE_LIMIT_RPS` | `rate_limit_rps` |
| `PT_LOG_LEVEL` | `log_level` |
| `PT_SETTINGS_FILE` | Path to settings file (default: `settings.json`) |

### Example Configurations

```bash
# Use settings.json with all defaults
./polytracker

# Override the settings file path
PT_SETTINGS_FILE=/etc/polytracker/prod.json ./polytracker

# Quick override without editing the file
PT_ALERT_THRESHOLD=0.03 PT_MIN_OI=5000 ./polytracker
```

---

## How It Works

```
Every 5m: refresh active market list and capture each market's open interest (Data API)

Every 60s per market (reusing the OI from the last refresh):
  ┌─ Fetch recent trades (Data API)
  ├─ For each new trade: is trade_USD / OI ≥ threshold?
  │   └─ YES → Apply optional signal-quality filters (min_trade_usd,
  │           max_signal_price, min_time_to_resolution) → Enrich with:
  │       ├─ Current midpoint price (CLOB API)
  │       ├─ Order book depth — top 5 bid/ask levels (CLOB API)
  │       ├─ Top holder check — is this wallet already a major holder? (Data API)
  │       └─ Open/close classification — open, add, reduce, or close? (Data API /positions)
  │       → Compute composite signalScore (size/room/time/action); drop if below min_score
  └─ Write structured JSON alert to the session log file + readable summary to stdout
```

**Key behaviors:**

- **Markets are refreshed every 5 minutes** — filtered by an OI floor and optional ceiling to skip illiquid or mega-markets
- **Trade deduplication** — timestamps are tracked per-market so the same trade is never flagged twice
- **Signal-quality filters & scoring** — flagged trades are annotated with `priceRoom`, `timeToResolutionDays`, `positionAction`, and a composite `signalScore`; optional hard filters (`min_trade_usd`, `max_signal_price`, `min_time_to_resolution`, `min_score`) drop weak signals. All default-off — zero behavior change until tuned.
- **Graceful degradation** — if enrichment calls fail (midpoint, book, holders, positions), the alert still fires with partial context
- **Clean shutdown** — `Ctrl+C` / `SIGTERM` drains in-flight checks without noisy errors

---

## Alert Format

Each whale trade is recorded two ways: a single-line structured JSON object (one per trade) is written to the **session log file** under `logs/`, and a plain-English summary is printed to **stdout**. Operational logs go to **stderr**. The JSON below is the shape written to the log file.

```json
{
  "alert": "WHALE_TRADE_DETECTED",
  "timestamp": "2026-06-09T21:50:00Z",
  "market": {
    "question": "Will X happen by Y?",
    "conditionId": "0xabc...",
    "slug": "will-x-happen"
  },
  "trade": {
    "size": 50000,
    "price": 0.65,
    "usdValue": 32500.00,
    "side": "BUY",
    "outcome": "Yes",
    "wallet": "0x1234...",
    "txHash": "0xdef..."
  },
  "context": {
    "openInterest": 500000.00,
    "tradeToOiRatio": 0.065,
    "thresholdPct": 5.0,
    "currentMidpoint": 0.66,
    "priceRoom": 0.35,
    "timeToResolutionDays": 124.5,
    "orderBookDepth": {
      "bestBid": "0.65",
      "bestAsk": "0.67",
      "bidDepth5Levels": 12500.00,
      "askDepth5Levels": 8300.00
    },
    "walletIsTopHolder": true,
    "walletHolderRank": 3,
    "walletHolderAmount": 120000,
    "positionAction": "OPEN",
    "walletAvgPrice": 0.63,
    "walletRealizedPnl": 0,
    "walletPositionSize": 50000,
    "signalScore": 73.5,
    "scoreBreakdown": {
      "size": 0.80,
      "room": 0.35,
      "time": 0.50,
      "action": 1.00
    }
  }
}
```

**Signal-quality annotations** (always present, used by the optional filters and the composite scorer):

- `priceRoom` — distance from the trade's entry price to certain resolution (`1 - price`). A 0.92 entry has `0.08` of room; lower = weaker signal.
- `timeToResolutionDays` — days until the market's resolution date (from Gamma `endDate`). Omitted when the date is unknown. Negative means already past resolution.
- `positionAction` — whether the trade `OPEN`ed, `INCREASE`d, `REDUCE`d, or `CLOSE`d the wallet's holding of that token (`UNKNOWN` if the positions lookup failed). Derived by comparing the trade against the wallet's current `/positions` snapshot. `walletAvgPrice` / `walletRealizedPnl` / `walletPositionSize` give the wallet's standing in that token. **Note:** the snapshot is read at alert time, not trade time, so classification can be off for wallets trading the same token several times within one poll interval.
- `signalScore` — composite 0–100 strength, a weighted blend of four normalized sub-signals (`scoreBreakdown`, each 0–1): **size** (`tradeToOiRatio`, saturating at `score_ref_ratio`), **room** (`priceRoom`), **time** (`timeToResolutionDays`, saturating at `score_ref_days`; unknown date → neutral 0.5, past resolution → 0), and **action** (`positionAction`: open 1.0 → close 0.1, unknown 0.5). Weights come from `score_weights` (auto-normalized). Tune the weights/refs to match what you consider a strong signal; set `min_score` to drop everything below a cutoff.

---

## Architecture

```
polytracker/
├── main.go          Orchestrator: polling loop, market refresh, graceful shutdown
├── config.go        Settings file loader with env var overrides
├── types.go         Data structures for API responses and alert payloads
├── api.go           HTTP client for Gamma, Data, and CLOB APIs (retry + backoff)
├── detector.go      Threshold detection, trade dedup, signal filters, scoring, enrichment
├── history.go       wallet-history command: realized P&L + win rate by price bucket
├── alerter.go       Structured JSON alert output via slog
├── settings.json    Configuration file (edit this)
└── go.mod           Module definition (stdlib only, no external deps)
```

### Polymarket APIs Used

| API | Base URL | Endpoints | Auth |
|-----|----------|-----------|------|
| **Gamma** (market discovery) | `gamma-api.polymarket.com` | `GET /markets` | None |
| **Data** (trades & analytics) | `data-api.polymarket.com` | `GET /trades`, `GET /oi`, `GET /holders`, `GET /positions` | None |
| **CLOB** (pricing & order book) | `clob.polymarket.com` | `GET /midpoint`, `GET /book` | None |

All endpoints are public and require no authentication.

### Rate Limiting

The service is designed to be API-friendly:

- **Concurrency is bounded** by a semaphore pool (`max_concurrency`, default 10)
- **Expensive calls** (midpoint, order book, holders, positions) only fire on flagged trades, not every market
- **Failed requests** retry with exponential backoff (500ms → 1s → 2s, max 3 attempts)
- **429/5xx responses** are retried automatically; on a `429` the server's `Retry-After` hint (capped at 10s) overrides the backoff. 4xx errors fail immediately

---

## Requirements

- **Go 1.22+** (uses `log/slog` and `min` builtin)
- **Network access** to `*.polymarket.com` — no authentication needed
- No external Go dependencies — stdlib only

---

## License

MIT
