package main

// alerter.go — Structured JSON alert output via slog.
// Each whale trade is emitted as a single JSON line to stdout,
// making it easy to pipe into log aggregators (Datadog, ELK, etc.).

import (
	"encoding/json"
	"log/slog"
	"os"
)

// Alerter handles formatting and emitting whale trade alerts.
type Alerter struct {
	logger *slog.Logger
}

// NewAlerter creates an Alerter that writes JSON to stdout.
// Uses a separate slog instance so alert output doesn't mix
// with operational logs (which go to stderr).
func NewAlerter() *Alerter {
	return &Alerter{
		logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			// Alerts are always at INFO level — never suppressed.
			Level: slog.LevelInfo,
		})),
	}
}

// EmitAlert writes a single whale trade alert as a JSON object to stdout.
// The entire WhaleTrade struct is serialized as the "alert_data" field
// within the slog JSON envelope.
func (a *Alerter) EmitAlert(alert WhaleTrade) {
	// Marshal the full alert struct so it nests cleanly in the slog output.
	raw, err := json.Marshal(alert)
	if err != nil {
		slog.Error("failed to marshal alert", "error", err)
		return
	}

	// Emit as a raw JSON message so consumers get the full structure.
	// Using RawMessage avoids double-encoding.
	a.logger.Info("WHALE_TRADE_DETECTED",
		"market", alert.Market.Question,
		"usd_value", alert.Trade.USDValue,
		"oi_ratio_pct", alert.Context.TradeToOIRatio*100,
		"wallet", alert.Trade.Wallet,
		"full_alert", json.RawMessage(raw),
	)
}

// EmitSummary logs a cycle summary — useful for health monitoring.
func (a *Alerter) EmitSummary(marketsChecked, alertsEmitted int) {
	a.logger.Info("poll_cycle_complete",
		"markets_checked", marketsChecked,
		"alerts_emitted", alertsEmitted,
	)
}
