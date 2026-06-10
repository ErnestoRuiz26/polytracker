package main

import (
	"os"
	"strconv"
	"time"
)

// Config holds all tunable parameters for the whale detector.
// Every field can be overridden via environment variables.
type Config struct {
	// AlertThreshold is the fraction of OI a single trade must exceed to trigger.
	// e.g. 0.05 = flag trades > 5% of the market's open interest.
	AlertThreshold float64

	// PollInterval controls how often we check each market for new trades.
	PollInterval time.Duration

	// MarketRefreshInterval controls how often the active market list is rebuilt.
	MarketRefreshInterval time.Duration

	// MaxConcurrency caps the number of markets polled simultaneously.
	MaxConcurrency int

	// MinOpenInterest filters out low-liquidity markets (USD floor).
	// Markets with OI below this value are skipped entirely.
	MinOpenInterest float64

	// MaxOpenInterest filters out mega-markets where large absolute bets
	// are routine and unlikely to be anomalous. Set to 0 to disable.
	MaxOpenInterest float64

	// MaxMarketsPerCycle caps how many markets we poll in a single cycle
	// to stay within rate limits.
	MaxMarketsPerCycle int

	// API base URLs — separated so they can be pointed at proxies or mocks.
	GammaBaseURL string
	DataBaseURL  string
	CLOBBaseURL  string
}

// LoadConfig reads configuration from environment variables, falling back
// to sensible defaults. We intentionally avoid flag parsing — this service
// is meant to run in containers where env vars are idiomatic.
func LoadConfig() *Config {
	return &Config{
		AlertThreshold:        envFloat("PT_ALERT_THRESHOLD", 0.05),
		PollInterval:          envDuration("PT_POLL_INTERVAL", 60*time.Second),
		MarketRefreshInterval: envDuration("PT_MARKET_REFRESH", 5*time.Minute),
		MaxConcurrency:        envInt("PT_MAX_CONCURRENCY", 10),
		MinOpenInterest:       envFloat("PT_MIN_OI", 10000),  // $10k floor
		MaxOpenInterest:       envFloat("PT_MAX_OI", 0),       // 0 = no ceiling
		MaxMarketsPerCycle:    envInt("PT_MAX_MARKETS", 500),
		GammaBaseURL:          envStr("PT_GAMMA_URL", "https://gamma-api.polymarket.com"),
		DataBaseURL:           envStr("PT_DATA_URL", "https://data-api.polymarket.com"),
		CLOBBaseURL:           envStr("PT_CLOB_URL", "https://clob.polymarket.com"),
	}
}

// --- env helpers (no external deps) ---

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
