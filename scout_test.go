package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadWatchlistCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchlist.json")

	wl, err := LoadWatchlist(path)
	if err != nil {
		t.Fatalf("LoadWatchlist: %v", err)
	}
	if wl.Len() != 0 {
		t.Errorf("Len = %d, want 0", wl.Len())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("watchlist file not created: %v", err)
	}
}

func TestWatchlistUpsertRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchlist.json")
	wl, err := LoadWatchlist(path)
	if err != nil {
		t.Fatalf("LoadWatchlist: %v", err)
	}

	isNew, err := wl.Upsert(WatchlistEntry{
		Wallet:      "0xABCDEF",
		Verdict:     verdictWatch,
		RealizedPnl: 1234.5,
		WinRate:     0.61,
		Resolved:    42,
		TriggeredBy: "Will X happen?",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !isNew {
		t.Error("first Upsert should report new")
	}

	// Reload from disk and verify persistence + lowercase keying.
	wl2, err := LoadWatchlist(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	e, ok := wl2.Get("0xabcdef")
	if !ok {
		t.Fatal("entry not found after reload")
	}
	if e.Verdict != verdictWatch || e.RealizedPnl != 1234.5 || e.Resolved != 42 {
		t.Errorf("entry mismatch: %+v", e)
	}
	if e.AddedAt == "" || e.UpdatedAt == "" {
		t.Errorf("timestamps not set: %+v", e)
	}

	// Second upsert: not new, AddedAt and original TriggeredBy preserved.
	isNew, err = wl2.Upsert(WatchlistEntry{
		Wallet:      "0xAbCdEf",
		Verdict:     verdictOK,
		TriggeredBy: "Different market",
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if isNew {
		t.Error("second Upsert should not report new")
	}
	e2, _ := wl2.Get("0xabcdef")
	if e2.AddedAt != e.AddedAt {
		t.Errorf("AddedAt changed: %q -> %q", e.AddedAt, e2.AddedAt)
	}
	if e2.TriggeredBy != "Will X happen?" {
		t.Errorf("TriggeredBy overwritten: %q", e2.TriggeredBy)
	}
	if e2.Verdict != verdictOK {
		t.Errorf("Verdict not refreshed: %q", e2.Verdict)
	}
}

// scoutForTest builds a Scout with a throwaway watchlist and no live client —
// fine for exercising Consider, which never touches the network.
func scoutForTest(t *testing.T) *Scout {
	t.Helper()
	wl, err := LoadWatchlist(filepath.Join(t.TempDir(), "watchlist.json"))
	if err != nil {
		t.Fatalf("LoadWatchlist: %v", err)
	}
	return NewScout(nil, defaults(), wl)
}

func alertWithStats(wallet string, stats *WalletStatsInfo) WhaleTrade {
	return WhaleTrade{
		Trade:   WhaleTradeLeg{Wallet: wallet},
		Market:  WhaleMarket{Question: "Q?"},
		Context: WhaleContext{WalletStats: stats},
	}
}

func TestScoutConsiderPrefilter(t *testing.T) {
	good := &WalletStatsInfo{TotalPnl: 500, WinRate: 0.60, Decided: 30}
	tests := []struct {
		name   string
		alert  WhaleTrade
		queued int
	}{
		{"passes prefilter", alertWithStats("0xA", good), 1},
		{"nil stats", alertWithStats("0xB", nil), 0},
		{"unprofitable", alertWithStats("0xC", &WalletStatsInfo{TotalPnl: -10, WinRate: 0.60, Decided: 30}), 0},
		{"weak win rate", alertWithStats("0xD", &WalletStatsInfo{TotalPnl: 500, WinRate: 0.40, Decided: 30}), 0},
		{"small sample", alertWithStats("0xE", &WalletStatsInfo{TotalPnl: 500, WinRate: 0.60, Decided: 5}), 0},
		{"empty wallet", alertWithStats("", good), 0},
	}
	for _, tt := range tests {
		s := scoutForTest(t)
		s.Consider([]WhaleTrade{tt.alert})
		if got := len(s.queue); got != tt.queued {
			t.Errorf("%s: queued = %d, want %d", tt.name, got, tt.queued)
		}
	}
}

func TestScoutConsiderDedupes(t *testing.T) {
	s := scoutForTest(t)
	a := alertWithStats("0xSAME", &WalletStatsInfo{TotalPnl: 500, WinRate: 0.60, Decided: 30})

	s.Consider([]WhaleTrade{a, a}) // same cycle
	s.Consider([]WhaleTrade{a})    // next cycle, within rescoutAfter
	if got := len(s.queue); got != 1 {
		t.Errorf("queued = %d, want 1 (deduped)", got)
	}
}

func TestScoutMarkScoutedExpiry(t *testing.T) {
	s := scoutForTest(t)
	if !s.markScouted("0xw") {
		t.Fatal("first mark should succeed")
	}
	if s.markScouted("0xw") {
		t.Error("second mark within TTL should fail")
	}
	// Age the record past the TTL; the wallet becomes due again.
	s.mu.Lock()
	s.scouted["0xw"] = time.Now().Add(-rescoutAfter - time.Minute)
	s.mu.Unlock()
	if !s.markScouted("0xw") {
		t.Error("mark after TTL should succeed")
	}
}
