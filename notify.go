package main

// notify.go — Discord push notifications for whale alerts.
//
// When discord_webhook_url is set (settings.json is gitignored, or use the
// PT_DISCORD_WEBHOOK env var), each whale alert is also pushed to a Discord
// channel as a rich embed. Posting is fully asynchronous: alerts are queued
// and drained by a single goroutine with a fixed gap between posts, so a slow
// or rate-limited webhook can never delay the poll loop. The webhook URL is
// the secret — it is never logged or printed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// notifyQueueSize bounds pending notifications; overflow is dropped with a
	// warning (the alert is still in the session log and on stdout).
	notifyQueueSize = 128
	// notifyPostGap spaces webhook posts to stay well under Discord's
	// ~30 requests/minute webhook limit.
	notifyPostGap = 2 * time.Second
	// notifyTimeout is the per-post HTTP timeout.
	notifyTimeout = 10 * time.Second
	// notifyMaxAttempts caps retries on a rate-limited (429) post.
	notifyMaxAttempts = 3

	// Discord embed colors: green for buys, red for sells, blue/grey for
	// lifecycle (start/stop) events.
	colorBuy   = 0x2ECC71
	colorSell  = 0xE74C3C
	colorStart = 0x3498DB
	colorStop  = 0x95A5A6
)

// discordPayload is the webhook request body.
type discordPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	URL         string         `json:"url,omitempty"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// DiscordNotifier pushes whale alerts to a Discord webhook. A nil notifier is
// valid and means "notifications off" — every method is nil-safe.
type DiscordNotifier struct {
	webhookURL string
	httpc      *http.Client
	queue      chan WhaleTrade
}

// NewDiscordNotifier returns a notifier for the webhook, or nil when the URL
// is empty (notifications disabled).
func NewDiscordNotifier(webhookURL string) *DiscordNotifier {
	if strings.TrimSpace(webhookURL) == "" {
		return nil
	}
	return &DiscordNotifier{
		webhookURL: webhookURL,
		httpc:      &http.Client{Timeout: notifyTimeout},
		queue:      make(chan WhaleTrade, notifyQueueSize),
	}
}

// Notify enqueues an alert for delivery. Non-blocking: when the queue is full
// the notification is dropped (the alert still reaches stdout and the log).
func (n *DiscordNotifier) Notify(alert WhaleTrade) {
	if n == nil {
		return
	}
	select {
	case n.queue <- alert:
	default:
		slog.Warn("discord notify queue full, dropping", "market", alert.Market.Question)
	}
}

// Run drains the queue until ctx is cancelled, spacing posts by notifyPostGap.
func (n *DiscordNotifier) Run(ctx context.Context) {
	if n == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case alert := <-n.queue:
			if err := n.post(ctx, alert); err != nil && ctx.Err() == nil {
				slog.Warn("discord notification failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(notifyPostGap):
			}
		}
	}
}

// NotifyEvent posts a lifecycle message (start/stop) as a simple embed.
// Unlike alerts it posts synchronously: the stop event fires while the process
// is exiting — after the queue drainer's context is already cancelled — so it
// cannot go through the async queue.
func (n *DiscordNotifier) NotifyEvent(title, description string, color int) {
	if n == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()

	payload := discordPayload{Embeds: []discordEmbed{{
		Title:       truncate(title, 256),
		Description: description,
		Color:       color,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}}}
	if err := n.postPayload(ctx, payload); err != nil {
		slog.Warn("discord lifecycle notification failed", "error", err)
	}
}

// post sends one alert to the webhook.
func (n *DiscordNotifier) post(ctx context.Context, alert WhaleTrade) error {
	return n.postPayload(ctx, buildDiscordPayload(alert))
}

// postPayload sends a payload to the webhook, honoring Retry-After on 429s.
func (n *DiscordNotifier) postPayload(ctx context.Context, payload discordPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	for attempt := 1; attempt <= notifyMaxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.webhookURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := n.httpc.Do(req)
		if err != nil {
			return fmt.Errorf("post webhook: %w", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode == http.StatusTooManyRequests && attempt < notifyMaxAttempts:
			delay := parseRetryAfter(resp.Header.Get("Retry-After"))
			if delay == 0 {
				delay = time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		default:
			// Deliberately omit the response body/URL from the error — the
			// webhook URL must never leak into logs.
			return fmt.Errorf("discord webhook returned HTTP %d", resp.StatusCode)
		}
	}
	return fmt.Errorf("discord webhook rate-limited after %d attempts", notifyMaxAttempts)
}

// buildDiscordPayload renders an alert as a Discord embed mirroring the
// stdout summary: trade size, outcome, entry vs current price, signal score,
// and the whale's track record when known.
func buildDiscordPayload(alert WhaleTrade) discordPayload {
	color := colorBuy
	if strings.EqualFold(alert.Trade.Side, "SELL") {
		color = colorSell
	}

	fields := []discordField{
		{Name: "Trade", Value: fmt.Sprintf("$%.2f %s %s", alert.Trade.USDValue, alert.Trade.Side, alert.Trade.Outcome), Inline: true},
		{Name: "Entry price", Value: fmt.Sprintf("%.3f (room %.3f)", alert.Trade.Price, alert.Context.PriceRoom), Inline: true},
		{Name: "Signal score", Value: fmt.Sprintf("%.0f/100", alert.Context.SignalScore), Inline: true},
	}
	if mid := alert.Context.CurrentMidpoint; mid > 0 {
		fields = append(fields, discordField{
			Name: "Current midpoint", Value: fmt.Sprintf("%.3f (drift %+.3f)", mid, mid-alert.Trade.Price), Inline: true,
		})
	}
	if a := alert.Context.PositionAction; a != "" && a != actionUnknown {
		fields = append(fields, discordField{Name: "Action", Value: a, Inline: true})
	}
	if d := alert.Context.TimeToResolutionDays; d != 0 {
		fields = append(fields, discordField{Name: "Resolves in", Value: fmt.Sprintf("%.1f days", d), Inline: true})
	}
	if ws := alert.Context.WalletStats; ws != nil && ws.Decided > 0 {
		fields = append(fields, discordField{
			Name:   "Whale track record",
			Value:  fmt.Sprintf("$%+.2f P&L, %.0f%% win rate (%d/%d positions)", ws.TotalPnl, ws.WinRate*100, ws.Decided, ws.Positions),
			Inline: false,
		})
	}
	fields = append(fields, discordField{Name: "Wallet", Value: alert.Trade.Wallet, Inline: false})

	return discordPayload{Embeds: []discordEmbed{{
		Title:     truncate("🐋 "+alert.Market.Question, 256),
		URL:       alert.Market.MarketURL,
		Color:     color,
		Fields:    fields,
		Timestamp: time.Unix(alert.Trade.Timestamp, 0).UTC().Format(time.RFC3339),
	}}}
}

// truncate shortens s to at most n bytes, appending an ellipsis when cut.
// The cut lands on a rune boundary so multi-byte characters aren't split.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
