package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// defaultSettingsFile is the path to the JSON config file.
// Override with the PT_SETTINGS_FILE env var to load from elsewhere.
const defaultSettingsFile = "settings.json"

// Config holds all tunable parameters for the whale detector.
// Values are loaded from settings.json, then overridden by any
// environment variables that are set.
type Config struct {
	// AlertThreshold is the fraction of OI a single trade must exceed to trigger.
	// e.g. 0.05 = flag trades > 5% of the market's open interest.
	AlertThreshold float64 `json:"alert_threshold"`

	// PollInterval controls how often we check each market for new trades.
	PollInterval Duration `json:"poll_interval"`

	// MarketRefreshInterval controls how often the active market list is rebuilt.
	MarketRefreshInterval Duration `json:"market_refresh_interval"`

	// MaxConcurrency caps the number of markets polled simultaneously.
	MaxConcurrency int `json:"max_concurrency"`

	// MinOpenInterest filters out low-liquidity markets (USD floor).
	// Markets with OI below this value are skipped entirely.
	MinOpenInterest float64 `json:"min_open_interest"`

	// MaxOpenInterest filters out mega-markets where large absolute bets
	// are routine and unlikely to be anomalous. Set to 0 to disable.
	MaxOpenInterest float64 `json:"max_open_interest"`

	// MaxMarketsPerCycle caps how many markets we poll in a single cycle
	// to stay within rate limits.
	MaxMarketsPerCycle int `json:"max_markets_per_cycle"`

	// API base URLs — separated so they can be pointed at proxies or mocks.
	GammaBaseURL string `json:"gamma_base_url"`
	DataBaseURL  string `json:"data_base_url"`
	CLOBBaseURL  string `json:"clob_base_url"`
}

// Duration wraps time.Duration so it can be unmarshaled from a JSON string
// like "60s" or "5m" instead of raw nanoseconds.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Fall back to numeric (nanoseconds) if not a string.
		var ns int64
		if err2 := json.Unmarshal(b, &ns); err2 != nil {
			return err
		}
		d.Duration = time.Duration(ns)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

// LoadConfig reads settings.json (if it exists), applies defaults for any
// missing fields, then lets environment variables override individual values.
// Precedence: env vars > settings.json > built-in defaults.
func LoadConfig() *Config {
	cfg := defaults()

	// Determine which settings file to read.
	path := envStr("PT_SETTINGS_FILE", defaultSettingsFile)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("no settings file found, using defaults", "path", path)
		} else {
			slog.Warn("failed to read settings file, using defaults", "path", path, "error", err)
		}
	} else {
		if err := json.Unmarshal(data, cfg); err != nil {
			slog.Warn("failed to parse settings file, using defaults", "path", path, "error", err)
		} else {
			slog.Info("loaded settings", "path", path)
		}
	}

	// Environment variables override file values (useful in containers).
	applyEnvOverrides(cfg)

	return cfg
}

// defaults returns a Config with sensible built-in values.
func defaults() *Config {
	return &Config{
		AlertThreshold:        0.05,
		PollInterval:          Duration{60 * time.Second},
		MarketRefreshInterval: Duration{5 * time.Minute},
		MaxConcurrency:        10,
		MinOpenInterest:       10000,
		MaxOpenInterest:       0,
		MaxMarketsPerCycle:    500,
		GammaBaseURL:          "https://gamma-api.polymarket.com",
		DataBaseURL:           "https://data-api.polymarket.com",
		CLOBBaseURL:           "https://clob.polymarket.com",
	}
}

// applyEnvOverrides lets env vars selectively override individual fields.
// Only fields with a corresponding env var set are touched.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("PT_ALERT_THRESHOLD"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.AlertThreshold = f
		}
	}
	if v := os.Getenv("PT_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.PollInterval = Duration{d}
		}
	}
	if v := os.Getenv("PT_MARKET_REFRESH"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.MarketRefreshInterval = Duration{d}
		}
	}
	if v := os.Getenv("PT_MAX_CONCURRENCY"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.MaxConcurrency = i
		}
	}
	if v := os.Getenv("PT_MIN_OI"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MinOpenInterest = f
		}
	}
	if v := os.Getenv("PT_MAX_OI"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.MaxOpenInterest = f
		}
	}
	if v := os.Getenv("PT_MAX_MARKETS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			cfg.MaxMarketsPerCycle = i
		}
	}
	if v := os.Getenv("PT_GAMMA_URL"); v != "" {
		cfg.GammaBaseURL = v
	}
	if v := os.Getenv("PT_DATA_URL"); v != "" {
		cfg.DataBaseURL = v
	}
	if v := os.Getenv("PT_CLOB_URL"); v != "" {
		cfg.CLOBBaseURL = v
	}
}

// --- env helper (only used for the settings file path itself) ---

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
