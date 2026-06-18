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

	// --- Signal-quality filters (all default 0 = disabled) ---

	// MinTradeUSD drops flagged trades whose absolute USD value is below this
	// floor. Kills tiny trades that only clear the OI ratio because the market
	// itself is small. 0 disables the floor.
	MinTradeUSD float64 `json:"min_trade_usd"`

	// MaxSignalPrice drops flagged trades executed above this price (0-1).
	// A trade near 1.0 has little room left to resolution, so it is a weak
	// signal. 0 disables the ceiling (trades are still annotated with priceRoom).
	MaxSignalPrice float64 `json:"max_signal_price"`

	// MinTimeToResolution drops flagged trades in markets resolving sooner than
	// this. Short-dated markets leave little time for a thesis to play out.
	// 0 disables the floor (alerts are still annotated with timeToResolutionDays).
	MinTimeToResolution Duration `json:"min_time_to_resolution"`

	// --- Composite signal score ---

	// ScoreWeights sets the relative weight of each sub-signal in the composite
	// signalScore. Weights are auto-normalized, so they need not sum to 1.
	ScoreWeights ScoreWeights `json:"score_weights"`

	// ScoreRefRatio is the trade/OI ratio at which the size sub-score saturates
	// to 1.0. e.g. 0.25 means a trade worth 25% of OI scores maximum on size.
	ScoreRefRatio float64 `json:"score_ref_ratio"`

	// ScoreRefDays is the time-to-resolution (days) at which the time sub-score
	// saturates to 1.0.
	ScoreRefDays float64 `json:"score_ref_days"`

	// MinScore drops flagged trades whose composite signalScore is below this
	// value (0-100). 0 disables the gate (alerts are still annotated with the score).
	MinScore float64 `json:"min_score"`

	// API base URLs — separated so they can be pointed at proxies or mocks.
	GammaBaseURL string `json:"gamma_base_url"`
	DataBaseURL  string `json:"data_base_url"`
	CLOBBaseURL  string `json:"clob_base_url"`

	// RateLimitRPS caps the total requests per second made by the client.
	RateLimitRPS int `json:"rate_limit_rps"`
}

// ScoreWeights holds the per-signal weights for the composite signalScore.
// Each field maps to one normalized sub-signal in computeScore.
type ScoreWeights struct {
	Size   float64 `json:"size"`   // weight of trade/OI ratio
	Room   float64 `json:"room"`   // weight of price room to resolution
	Time   float64 `json:"time"`   // weight of time to resolution
	Action float64 `json:"action"` // weight of position action (open/close)
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
		RateLimitRPS:          10,
		ScoreWeights: ScoreWeights{
			Size:   0.40,
			Room:   0.15,
			Time:   0.15,
			Action: 0.30,
		},
		ScoreRefRatio: 0.25,
		ScoreRefDays:  30,
		MinScore:      0,
	}
}

// applyEnvOverrides lets env vars selectively override individual fields.
// Only fields with a corresponding env var set (and parseable) are touched.
func applyEnvOverrides(cfg *Config) {
	envFloat("PT_ALERT_THRESHOLD", &cfg.AlertThreshold)
	envDuration("PT_POLL_INTERVAL", &cfg.PollInterval)
	envDuration("PT_MARKET_REFRESH", &cfg.MarketRefreshInterval)
	envInt("PT_MAX_CONCURRENCY", &cfg.MaxConcurrency)
	envFloat("PT_MIN_OI", &cfg.MinOpenInterest)
	envFloat("PT_MAX_OI", &cfg.MaxOpenInterest)
	envInt("PT_MAX_MARKETS", &cfg.MaxMarketsPerCycle)
	envFloat("PT_MIN_TRADE_USD", &cfg.MinTradeUSD)
	envFloat("PT_MAX_SIGNAL_PRICE", &cfg.MaxSignalPrice)
	envDuration("PT_MIN_TIME_TO_RESOLUTION", &cfg.MinTimeToResolution)
	envFloat("PT_SCORE_WEIGHT_SIZE", &cfg.ScoreWeights.Size)
	envFloat("PT_SCORE_WEIGHT_ROOM", &cfg.ScoreWeights.Room)
	envFloat("PT_SCORE_WEIGHT_TIME", &cfg.ScoreWeights.Time)
	envFloat("PT_SCORE_WEIGHT_ACTION", &cfg.ScoreWeights.Action)
	envFloat("PT_SCORE_REF_RATIO", &cfg.ScoreRefRatio)
	envFloat("PT_SCORE_REF_DAYS", &cfg.ScoreRefDays)
	envFloat("PT_MIN_SCORE", &cfg.MinScore)
	envStrInto("PT_GAMMA_URL", &cfg.GammaBaseURL)
	envStrInto("PT_DATA_URL", &cfg.DataBaseURL)
	envStrInto("PT_CLOB_URL", &cfg.CLOBBaseURL)
	envInt("PT_RATE_LIMIT_RPS", &cfg.RateLimitRPS)
}

// --- env helpers: each assigns to *dst only when the var is set and parses ---

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envStrInto(key string, dst *string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envInt(key string, dst *int) {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			*dst = i
		}
	}
}

func envFloat(key string, dst *float64) {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			*dst = f
		}
	}
}

func envDuration(key string, dst *Duration) {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = Duration{d}
		}
	}
}
