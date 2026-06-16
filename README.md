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
```

When tracking a specific wallet, the tool retrieves and paginates historical trades 10 at a time (sorted latest to oldest). Pressing `[Enter]` transitions to real-time tracking, polling every `poll_interval`. Log files are saved under `logs/` with names following `session_command_flag_DATE_TIME.log`.

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
  "gamma_base_url": "https://gamma-api.polymarket.com",
  "data_base_url": "https://data-api.polymarket.com",
  "clob_base_url": "https://clob.polymarket.com"
}
```

### Settings Reference

| Field | Default | Description |
|-------|---------|-------------|
| `alert_threshold` | `0.05` | Trade/OI ratio to trigger alert (0.05 = 5%) |
| `poll_interval` | `"60s"` | How often to check each market for new trades |
| `market_refresh_interval` | `"5m"` | How often to rebuild the active market list |
| `max_concurrency` | `10` | Max parallel API calls |
| `min_open_interest` | `10000` | Minimum open interest (USD) — markets below this are ignored |
| `max_open_interest` | `0` | Maximum open interest (USD) — `0` means no upper limit |
| `max_markets_per_cycle` | `500` | Cap on total markets to monitor |
| `gamma_base_url` | `"https://gamma-api.polymarket.com"` | Gamma API base URL |
| `data_base_url` | `"https://data-api.polymarket.com"` | Data API base URL |
| `clob_base_url` | `"https://clob.polymarket.com"` | CLOB API base URL |

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
| `PT_GAMMA_URL` | `gamma_base_url` |
| `PT_DATA_URL` | `data_base_url` |
| `PT_CLOB_URL` | `clob_base_url` |
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
  │   └─ YES → Enrich with:
  │       ├─ Current midpoint price (CLOB API)
  │       ├─ Order book depth — top 5 bid/ask levels (CLOB API)
  │       └─ Top holder check — is this wallet already a major holder? (Data API)
  └─ Write structured JSON alert to the session log file + readable summary to stdout
```

**Key behaviors:**
- **Markets are refreshed every 5 minutes** — filtered by an OI floor and optional ceiling to skip illiquid or mega-markets
- **Trade deduplication** — timestamps are tracked per-market so the same trade is never flagged twice
- **Graceful degradation** — if enrichment calls fail (midpoint, book, holders), the alert still fires with partial context
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
    "orderBookDepth": {
      "bestBid": "0.65",
      "bestAsk": "0.67",
      "bidDepth5Levels": 12500.00,
      "askDepth5Levels": 8300.00
    },
    "walletIsTopHolder": true,
    "walletHolderRank": 3,
    "walletHolderAmount": 120000
  }
}
```

### Filtering the JSON alerts

Structured alerts land in the session log file under `logs/` as one slog JSON
line per trade, with the full alert nested under `full_alert`. Extract and
filter them with `jq`:

```bash
# Pull the nested alert payloads out of the newest session log
jq -c 'select(.msg == "WHALE_TRADE_DETECTED") | .full_alert' logs/session_*.log

# Filter for trades above $50K
jq -c 'select(.msg == "WHALE_TRADE_DETECTED") | .full_alert
       | select(.trade.usdValue > 50000)' logs/session_*.log

# Watch for repeat accumulation (wallet is already a top holder)
jq -c 'select(.msg == "WHALE_TRADE_DETECTED") | .full_alert
       | select(.context.walletIsTopHolder == true)' logs/session_*.log
```

---

## Architecture

```
polytracker/
├── main.go          Orchestrator: polling loop, market refresh, graceful shutdown
├── config.go        Settings file loader with env var overrides
├── types.go         Data structures for API responses and alert payloads
├── api.go           HTTP client for Gamma, Data, and CLOB APIs (retry + backoff)
├── detector.go      Threshold detection, trade dedup, enrichment pipeline
├── alerter.go       Structured JSON alert output via slog
├── settings.json    Configuration file (edit this)
└── go.mod           Module definition (stdlib only, no external deps)
```

### Polymarket APIs Used

| API | Base URL | Endpoints | Auth |
|-----|----------|-----------|------|
| **Gamma** (market discovery) | `gamma-api.polymarket.com` | `GET /markets` | None |
| **Data** (trades & analytics) | `data-api.polymarket.com` | `GET /trades`, `GET /oi`, `GET /holders` | None |
| **CLOB** (pricing & order book) | `clob.polymarket.com` | `GET /midpoint`, `GET /book` | None |

All endpoints are public and require no authentication.

### Rate Limiting

The service is designed to be API-friendly:
- **Concurrency is bounded** by a semaphore pool (`max_concurrency`, default 10)
- **Expensive calls** (midpoint, order book, holders) only fire on flagged trades, not every market
- **Failed requests** retry with exponential backoff (500ms → 1s → 2s, max 3 attempts)
- **429/5xx responses** are retried automatically; 4xx errors fail immediately

---

## Requirements

- **Go 1.22+** (uses `log/slog` and `min` builtin)
- **Network access** to `*.polymarket.com` — no authentication needed
- No external Go dependencies — stdlib only

---

## License

MIT
