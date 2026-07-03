package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewDiscordNotifierDisabled(t *testing.T) {
	if n := NewDiscordNotifier(""); n != nil {
		t.Error("empty URL should return nil notifier")
	}
	if n := NewDiscordNotifier("   "); n != nil {
		t.Error("blank URL should return nil notifier")
	}

	// Nil notifier must be safe to use.
	var n *DiscordNotifier
	n.Notify(WhaleTrade{})
	n.NotifyEvent("start", "detail", colorStart)
	n.Run(context.Background()) // returns immediately
}

func TestNotifyEventPosts(t *testing.T) {
	var got discordPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := NewDiscordNotifier(srv.URL)
	n.NotifyEvent("🟢 Polytracker started", "Command: `track`", colorStart)

	if len(got.Embeds) != 1 {
		t.Fatalf("embeds = %d, want 1", len(got.Embeds))
	}
	e := got.Embeds[0]
	if !strings.Contains(e.Title, "started") || !strings.Contains(e.Description, "track") {
		t.Errorf("unexpected embed: %+v", e)
	}
	if e.Color != colorStart {
		t.Errorf("color = %#x, want start blue", e.Color)
	}
}

func sampleAlert() WhaleTrade {
	return WhaleTrade{
		Market: WhaleMarket{
			Question:  "Will X happen?",
			MarketURL: "https://polymarket.com/market/will-x-happen",
		},
		Trade: WhaleTradeLeg{
			USDValue:  12500,
			Side:      "BUY",
			Outcome:   "Yes",
			Price:     0.42,
			Wallet:    "0xabc",
			Timestamp: 1783110254,
		},
		Context: WhaleContext{
			PriceRoom:       0.58,
			SignalScore:     81,
			CurrentMidpoint: 0.44,
			PositionAction:  actionOpen,
			WalletStats:     &WalletStatsInfo{TotalPnl: 5000, WinRate: 0.6, Decided: 40, Positions: 50},
		},
	}
}

func TestBuildDiscordPayload(t *testing.T) {
	p := buildDiscordPayload(sampleAlert())
	if len(p.Embeds) != 1 {
		t.Fatalf("embeds = %d, want 1", len(p.Embeds))
	}
	e := p.Embeds[0]
	if !strings.Contains(e.Title, "Will X happen?") {
		t.Errorf("title missing question: %q", e.Title)
	}
	if e.Color != colorBuy {
		t.Errorf("color = %#x, want buy green", e.Color)
	}

	joined := ""
	for _, f := range e.Fields {
		joined += f.Name + "=" + f.Value + ";"
	}
	for _, want := range []string{"$12500.00 BUY Yes", "0.420", "81/100", "drift +0.020", "OPEN", "60% win rate", "0xabc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("fields missing %q in %q", want, joined)
		}
	}

	// SELL flips the color.
	a := sampleAlert()
	a.Trade.Side = "SELL"
	if got := buildDiscordPayload(a).Embeds[0].Color; got != colorSell {
		t.Errorf("sell color = %#x, want red", got)
	}
}

func TestDiscordPostDeliversAndRetries429(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var p discordPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("bad payload: %v", err)
		}
		if len(p.Embeds) == 0 || !strings.Contains(p.Embeds[0].Title, "Will X happen?") {
			t.Errorf("unexpected payload: %s", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := NewDiscordNotifier(srv.URL)
	if err := n.post(context.Background(), sampleAlert()); err != nil {
		t.Fatalf("post: %v", err)
	}
	if requests != 2 {
		t.Errorf("requests = %d, want 2 (one 429 retry)", requests)
	}
}

func TestDiscordPostFailsFastOnClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	n := NewDiscordNotifier(srv.URL)
	err := n.post(context.Background(), sampleAlert())
	if err == nil {
		t.Fatal("expected error on 400")
	}
	// The error must not leak the webhook URL.
	if strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks webhook URL: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("no-op truncate = %q", got)
	}
	if got := truncate("hello world", 8); len(got) > 8 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncate = %q", got)
	}
}
