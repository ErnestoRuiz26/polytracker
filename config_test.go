package main

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg := defaults()
	if cfg.AlertThreshold != 0.05 {
		t.Errorf("AlertThreshold = %v, want 0.05", cfg.AlertThreshold)
	}
	if cfg.PollInterval.Duration != 60*time.Second {
		t.Errorf("PollInterval = %v, want 60s", cfg.PollInterval.Duration)
	}
	if cfg.MarketRefreshInterval.Duration != 5*time.Minute {
		t.Errorf("MarketRefreshInterval = %v, want 5m", cfg.MarketRefreshInterval.Duration)
	}
	if cfg.MaxConcurrency != 10 || cfg.RateLimitRPS != 10 {
		t.Errorf("concurrency/rps mismatch: %+v", cfg)
	}
	if cfg.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn", cfg.LogLevel)
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := map[string]int{
		"debug": -4, "info": 0, "warn": 4, "error": 8,
		"WARN": 4, "": 4, "garbage": 4, " info ": 0,
	}
	for in, want := range tests {
		if got := int(parseLogLevel(in)); got != want {
			t.Errorf("parseLogLevel(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestDurationUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"string seconds", `"60s"`, 60 * time.Second, false},
		{"string compound", `"1h30m"`, 90 * time.Minute, false},
		{"numeric nanoseconds", `1000000000`, time.Second, false},
		{"garbage string", `"notaduration"`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d Duration
			err := d.UnmarshalJSON([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d.Duration != tt.want {
				t.Errorf("Duration = %v, want %v", d.Duration, tt.want)
			}
		})
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	cfg := defaults()

	t.Setenv("PT_ALERT_THRESHOLD", "0.03")
	t.Setenv("PT_POLL_INTERVAL", "30s")
	t.Setenv("PT_MAX_CONCURRENCY", "25")
	t.Setenv("PT_MIN_OI", "5000")
	t.Setenv("PT_GAMMA_URL", "https://example.test")
	// Unparseable values must be ignored, leaving the default intact.
	t.Setenv("PT_MAX_MARKETS", "notanint")

	applyEnvOverrides(cfg)

	if cfg.AlertThreshold != 0.03 {
		t.Errorf("AlertThreshold = %v, want 0.03", cfg.AlertThreshold)
	}
	if cfg.PollInterval.Duration != 30*time.Second {
		t.Errorf("PollInterval = %v, want 30s", cfg.PollInterval.Duration)
	}
	if cfg.MaxConcurrency != 25 {
		t.Errorf("MaxConcurrency = %v, want 25", cfg.MaxConcurrency)
	}
	if cfg.MinOpenInterest != 5000 {
		t.Errorf("MinOpenInterest = %v, want 5000", cfg.MinOpenInterest)
	}
	if cfg.GammaBaseURL != "https://example.test" {
		t.Errorf("GammaBaseURL = %v", cfg.GammaBaseURL)
	}
	if cfg.MaxMarketsPerCycle != 500 {
		t.Errorf("MaxMarketsPerCycle = %v, want default 500 (bad env ignored)", cfg.MaxMarketsPerCycle)
	}
}
