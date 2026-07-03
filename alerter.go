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

// EmitAlert records a whale trade: the full structured JSON goes to the
// session log file, and a human-readable summary is printed to stdout.
func (a *Alerter) EmitAlert(alert WhaleTrade) {
	a.logJSON(alert)
	printSummary(alert)
}

// logJSON writes the full alert struct as a single JSON line to the log file.
func (a *Alerter) logJSON(alert WhaleTrade) {
	// Marshal the full alert struct so it nests cleanly in the slog output.
	raw, err := json.Marshal(alert)
	if err != nil {
		slog.Error("failed to marshal alert", "error", err)
		return
	}

	// Using RawMessage avoids double-encoding the nested payload.
	a.logger.Info("WHALE_TRADE_DETECTED",
		"market", alert.Market.Question,
		"usd_value", alert.Trade.USDValue,
		"oi_ratio_pct", alert.Context.TradeToOIRatio*100,
		"wallet", alert.Trade.Wallet,
		"full_alert", json.RawMessage(raw),
	)
}

// printSummary prints a readable, plain-English summary of an alert to stdout.
func printSummary(alert WhaleTrade) {
	placedTime := time.Unix(alert.Trade.Timestamp, 0).UTC().Format("2006-01-02 15:04:05 MST")
	fmt.Printf("__________________________________________________\n")
	fmt.Printf("USD Value of position: $%.2f\n", alert.Trade.USDValue)
	fmt.Printf("Market Name:           %s\n", alert.Market.Question)
	fmt.Printf("Placed At:             %s\n", placedTime)
	fmt.Printf("Wallet:                %s\n", alert.Trade.Wallet)
	fmt.Printf("Side:                  %s\n", alert.Trade.Side)
	if alert.Context.PositionAction != "" && alert.Context.PositionAction != actionUnknown {
		fmt.Printf("Position action:       %s (avg entry %.3f, realized P&L $%.2f)\n",
			alert.Context.PositionAction, alert.Context.WalletAvgPrice, alert.Context.WalletRealizedPnl)
	}
	if ws := alert.Context.WalletStats; ws != nil && ws.Decided > 0 {
		fmt.Printf("Whale track record:    $%+.2f P&L, %.0f%% win rate (%d/%d decided positions)\n",
			ws.TotalPnl, ws.WinRate*100, ws.Decided, ws.Positions)
	}
	fmt.Printf("Predicted outcome:     %s\n", alert.Trade.Outcome)
	fmt.Printf("Entry price:           %.3f  (room to 1.0: %.3f)\n", alert.Trade.Price, alert.Context.PriceRoom)
	if mid := alert.Context.CurrentMidpoint; mid > 0 {
		fmt.Printf("Current midpoint:      %.3f  (drift since whale entry: %+.3f)\n",
			mid, mid-alert.Trade.Price)
	}
	if alert.Context.TimeToResolutionDays != 0 {
		fmt.Printf("Resolves in:           %.1f days\n", alert.Context.TimeToResolutionDays)
	}
	b := alert.Context.ScoreBreakdown
	fmt.Printf("Signal score:          %.0f/100 (size %.2f room %.2f time %.2f action %.2f)\n",
		alert.Context.SignalScore, b["size"], b["room"], b["time"], b["action"])
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
