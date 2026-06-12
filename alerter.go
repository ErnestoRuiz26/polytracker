package main

// alerter.go — Structured JSON alert output via slog.
// Each whale trade is emitted as a single JSON line to stdout,
// making it easy to pipe into log aggregators (Datadog, ELK, etc.).

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Alerter handles formatting and emitting whale trade alerts.
type Alerter struct {
	logger *slog.Logger
}

// NewAlerter creates an Alerter that writes JSON to the provided writer.
func NewAlerter(w io.Writer) *Alerter {
	return &Alerter{
		logger: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
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

	// Output readable summary in simple English to the terminal
	placedTime := time.Unix(alert.Trade.Timestamp, 0).UTC().Format("2006-01-02 15:04:05 MST")
	fmt.Printf("__________________________________________________\n")
	fmt.Printf("USD Value of position: $%.2f\n", alert.Trade.USDValue)
	fmt.Printf("Market Name:           %s\n", alert.Market.Question)
	fmt.Printf("Placed At:             %s\n", placedTime)
	fmt.Printf("Wallet:                %s\n", alert.Trade.Wallet)
	fmt.Printf("Side:                  %s\n", alert.Trade.Side)
	fmt.Printf("Predicted outcome:     %s\n", alert.Trade.Outcome)
	fmt.Printf("Market Link:           %s\n", alert.Market.MarketURL)
	fmt.Printf("__________________________________________________\n\n")
}

// EmitSummary logs a cycle summary — useful for health monitoring.
func (a *Alerter) EmitSummary(marketsChecked, alertsEmitted int) {
	a.logger.Info("poll_cycle_complete",
		"markets_checked", marketsChecked,
		"alerts_emitted", alertsEmitted,
	)
}
