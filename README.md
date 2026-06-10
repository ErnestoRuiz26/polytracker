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
./polytracker
```

That's it. No API keys, no config files, no external dependencies. The service starts monitoring all active Polymarket markets with open interest ≥ $10K and flags any single trade exceeding 5% of the market's OI.

---

## How It Works

```
Every 60s per market:
  ┌─ Fetch recent trades (Data API)
  ├─ Fetch open interest (Data API)
  ├─ For each trade: is trade_USD / OI ≥ threshold?
  │   └─ YES → Enrich with:
  │       ├─ Current midpoint price (CLOB API)
  │       ├─ Order book depth — top 5 bid/ask levels (CLOB API)
  │       └─ Top holder check — is this wallet already a major holder? (Data API)
  └─ Emit structured JSON alert to stdout
```

**Key behaviors:**
- **Markets are refreshed every 5 minutes** — filtered by an OI floor and optional ceiling to skip illiquid or mega-markets
- **Trade deduplication** — timestamps are tracked per-market so the same trade is never flagged twice
- **Graceful degradation** — if enrichment calls fail (midpoint, book, holders), the alert still fires with partial context
- **Clean shutdown** — `Ctrl+C` / `SIGTERM` drains in-flight checks without noisy errors

---

## Alert Format

Alerts are written to **stdout** as single-line JSON (one per trade). Operational logs go to **stderr**.

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

This makes it easy to pipe into log aggregators (Datadog, ELK, Loki) or downstream scripts:

```bash
# Save alerts to a file, keep operational logs on screen
./polytracker > alerts.jsonl

# Filter for trades above $50K
./polytracker | jq 'select(.trade.usdValue > 50000)'

# Watch for repeat accumulation (wallet is already a top holder)
./polytracker | jq 'select(.context.walletIsTopHolder == true)'
```

---

## Configuration

All settings are controlled via environment variables. No config files needed.

| Variable | Default | Description |
|----------|---------|-------------|
| `PT_ALERT_THRESHOLD` | `0.05` | Trade/OI ratio to trigger alert (0.05 = 5%) |
| `PT_POLL_INTERVAL` | `60s` | How often to check each market for new trades |
| `PT_MARKET_REFRESH` | `5m` | How often to rebuild the active market list |
| `PT_MAX_CONCURRENCY` | `10` | Max parallel API calls |
| `PT_MIN_OI` | `10000` | Minimum open interest in USD — markets below this are ignored |
| `PT_MAX_OI` | `0` | Maximum open interest in USD — 0 means no upper limit |
| `PT_MAX_MARKETS` | `500` | Cap on total markets to monitor |
| `PT_GAMMA_URL` | `https://gamma-api.polymarket.com` | Gamma API base URL |
| `PT_DATA_URL` | `https://data-api.polymarket.com` | Data API base URL |
| `PT_CLOB_URL` | `https://clob.polymarket.com` | CLOB API base URL |

### Examples

```bash
# Lower threshold to 3%, only watch markets with OI between $5K and $500K
PT_ALERT_THRESHOLD=0.03 PT_MIN_OI=5000 PT_MAX_OI=500000 ./polytracker

# Poll more aggressively (every 30s), higher concurrency
PT_POLL_INTERVAL=30s PT_MAX_CONCURRENCY=20 ./polytracker

# Durations accept Go duration strings: "30s", "2m", "1h"
PT_POLL_INTERVAL=30s PT_MARKET_REFRESH=10m ./polytracker
```

---

## Architecture

```
polytracker/
├── main.go        Orchestrator: polling loop, market refresh, graceful shutdown
├── config.go      Environment-based configuration with sensible defaults
├── types.go       Data structures for API responses and alert payloads
├── api.go         HTTP client for Gamma, Data, and CLOB APIs (retry + backoff)
├── detector.go    Threshold detection, trade dedup, enrichment pipeline
├── alerter.go     Structured JSON alert output via slog
└── go.mod         Module definition (stdlib only, no external deps)
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
- **Concurrency is bounded** by a semaphore pool (`PT_MAX_CONCURRENCY`, default 10)
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
