package main

import (
	"math"
	"testing"
	"time"
)

func TestPriceRoom(t *testing.T) {
	tests := []struct {
		price, want float64
	}{
		{0.65, 0.35},
		{0.92, 0.08},
		{0.0, 1.0},
		{1.0, 0.0},
		{1.2, 0.0},  // malformed > 1 clamps to 0
		{-0.1, 1.0}, // malformed < 0 clamps to 1
	}
	for _, tt := range tests {
		if got := priceRoom(tt.price); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("priceRoom(%v) = %v, want %v", tt.price, got, tt.want)
		}
	}
}

func TestDaysUntil(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t.Run("zero end is unknown", func(t *testing.T) {
		if got := daysUntil(time.Time{}, now); got != 0 {
			t.Errorf("daysUntil(zero) = %v, want 0", got)
		}
	})
	t.Run("ten days out", func(t *testing.T) {
		end := now.Add(10 * 24 * time.Hour)
		if got := daysUntil(end, now); got != 10 {
			t.Errorf("daysUntil = %v, want 10", got)
		}
	})
	t.Run("past resolution is negative", func(t *testing.T) {
		end := now.Add(-2 * 24 * time.Hour)
		if got := daysUntil(end, now); got != -2 {
			t.Errorf("daysUntil = %v, want -2", got)
		}
	})
}

func TestPassesQuickFilters(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end30d := now.Add(30 * 24 * time.Hour)

	tests := []struct {
		name       string
		cfg        *Config
		usd, price float64
		end        time.Time
		want       bool
	}{
		{"all disabled passes", &Config{}, 1, 0.99, time.Time{}, true},
		{"below USD floor drops", &Config{MinTradeUSD: 5000}, 4999, 0.5, end30d, false},
		{"at USD floor passes", &Config{MinTradeUSD: 5000}, 5000, 0.5, end30d, true},
		{"above price ceiling drops", &Config{MaxSignalPrice: 0.90}, 9999, 0.91, end30d, false},
		{"at price ceiling passes", &Config{MaxSignalPrice: 0.90}, 9999, 0.90, end30d, true},
		{"too soon to resolve drops", &Config{MinTimeToResolution: Duration{7 * 24 * time.Hour}}, 9999, 0.5, now.Add(24 * time.Hour), false},
		{"far enough out passes", &Config{MinTimeToResolution: Duration{7 * 24 * time.Hour}}, 9999, 0.5, end30d, true},
		{"unknown end skips time filter", &Config{MinTimeToResolution: Duration{7 * 24 * time.Hour}}, 9999, 0.5, time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := passesQuickFilters(tt.cfg, tt.usd, tt.price, tt.end, now); got != tt.want {
				t.Errorf("passesQuickFilters = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMarketEndTime(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		wantOK bool
	}{
		{"rfc3339", "2026-12-31T12:00:00Z", true},
		{"date only", "2026-12-31", true},
		{"empty is zero", "", false},
		{"garbage is zero", "not-a-date", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&Market{EndDateRaw: tt.raw}).EndTime()
			if tt.wantOK && got.IsZero() {
				t.Errorf("EndTime(%q) = zero, want parsed", tt.raw)
			}
			if !tt.wantOK && !got.IsZero() {
				t.Errorf("EndTime(%q) = %v, want zero", tt.raw, got)
			}
		})
	}
}

func TestSumDepth(t *testing.T) {
	levels := []OrderBookLevel{
		{Price: "0.50", Size: "100"}, // 50
		{Price: "0.40", Size: "100"}, // 40
		{Price: "0.30", Size: "100"}, // 30
	}
	tests := []struct {
		name   string
		levels []OrderBookLevel
		n      int
		want   float64
	}{
		{"fewer than n", levels, 5, 120},
		{"exactly n", levels, 3, 120},
		{"more than n caps", levels, 2, 90},
		{"zero n", levels, 0, 0},
		{"unparseable skipped as zero", []OrderBookLevel{{Price: "x", Size: "y"}, {Price: "0.5", Size: "10"}}, 5, 5},
		{"empty", nil, 5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sumDepth(tt.levels, tt.n); got != tt.want {
				t.Errorf("sumDepth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSummarizeBook(t *testing.T) {
	t.Run("empty book", func(t *testing.T) {
		info := summarizeBook(&OrderBook{})
		if info.BestBid != "" || info.BestAsk != "" || info.BidDepth5 != 0 || info.AskDepth5 != 0 {
			t.Errorf("empty book should yield zero values, got %+v", info)
		}
	})
	t.Run("populated", func(t *testing.T) {
		book := &OrderBook{
			Bids: []OrderBookLevel{{Price: "0.64", Size: "100"}},
			Asks: []OrderBookLevel{{Price: "0.66", Size: "50"}},
		}
		info := summarizeBook(book)
		if info.BestBid != "0.64" || info.BestAsk != "0.66" {
			t.Errorf("best bid/ask mismatch: %+v", info)
		}
		if info.BidDepth5 != 64 || info.AskDepth5 != 33 {
			t.Errorf("depth mismatch: %+v", info)
		}
	})
}

func TestFindHolder(t *testing.T) {
	groups := []HolderGroup{
		{Token: "tokA", Holders: []Holder{
			{ProxyWallet: "0xAAA", Amount: 100},
			{ProxyWallet: "0xBBB", Amount: 50},
		}},
		{Token: "tokB", Holders: []Holder{
			{ProxyWallet: "0xCCC", Amount: 25},
		}},
	}
	t.Run("case-insensitive match", func(t *testing.T) {
		rank, amount, found := findHolder(groups, "0xbbb")
		if !found || rank != 2 || amount != 50 {
			t.Errorf("got rank=%d amount=%v found=%v", rank, amount, found)
		}
	})
	t.Run("match in second group", func(t *testing.T) {
		rank, amount, found := findHolder(groups, "0xCCC")
		if !found || rank != 1 || amount != 25 {
			t.Errorf("got rank=%d amount=%v found=%v", rank, amount, found)
		}
	})
	t.Run("miss", func(t *testing.T) {
		_, _, found := findHolder(groups, "0xZZZ")
		if found {
			t.Error("expected not found")
		}
	})
}

func TestNewTradesSince(t *testing.T) {
	trades := []Trade{
		{Timestamp: 100},
		{Timestamp: 200},
		{Timestamp: 150},
	}
	t.Run("filters older, reports max", func(t *testing.T) {
		newer, maxTS := newTradesSince(trades, 150)
		if maxTS != 200 {
			t.Errorf("maxTS = %d, want 200", maxTS)
		}
		if len(newer) != 1 || newer[0].Timestamp != 200 {
			t.Errorf("newer = %+v, want only ts=200", newer)
		}
	})
	t.Run("all new from zero checkpoint", func(t *testing.T) {
		newer, maxTS := newTradesSince(trades, 0)
		if len(newer) != 3 || maxTS != 200 {
			t.Errorf("newer=%d maxTS=%d", len(newer), maxTS)
		}
	})
	t.Run("none new when checkpoint at max", func(t *testing.T) {
		newer, maxTS := newTradesSince(trades, 200)
		if len(newer) != 0 || maxTS != 200 {
			t.Errorf("newer=%d maxTS=%d", len(newer), maxTS)
		}
	})
}

// TestSetWalletCheckpoint verifies the checkpoint only advances forward,
// guarding the wallet-mode handoff from historical to real-time.
func TestSetWalletCheckpoint(t *testing.T) {
	d := NewDetector(nil, nil)
	d.SetWalletCheckpoint("0xWALLET", 100)
	if got := d.seenWallet["0xWALLET"]; got != 100 {
		t.Fatalf("checkpoint = %d, want 100", got)
	}
	// Lower value must not regress the checkpoint.
	d.SetWalletCheckpoint("0xWALLET", 50)
	if got := d.seenWallet["0xWALLET"]; got != 100 {
		t.Fatalf("checkpoint regressed to %d, want 100", got)
	}
	// Higher value advances it.
	d.SetWalletCheckpoint("0xWALLET", 250)
	if got := d.seenWallet["0xWALLET"]; got != 250 {
		t.Fatalf("checkpoint = %d, want 250", got)
	}
}
