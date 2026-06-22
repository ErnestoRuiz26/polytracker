package main

import (
	"math"
	"testing"
)

func TestBucketIndex(t *testing.T) {
	tests := []struct {
		price float64
		want  int
	}{
		{0.0, 0},
		{0.05, 0},
		{0.10, 1},
		{0.19, 1},
		{0.55, 5},
		{0.90, 9},
		{0.99, 9},
		{1.0, 9},  // top edge clamps into last bucket
		{1.5, 9},  // malformed > 1 clamps
		{-0.2, 0}, // malformed < 0 clamps
	}
	for _, tt := range tests {
		if got := bucketIndex(tt.price); got != tt.want {
			t.Errorf("bucketIndex(%v) = %d, want %d", tt.price, got, tt.want)
		}
	}
}

func TestNewBuckets(t *testing.T) {
	b := newBuckets()
	if len(b) != bucketCount {
		t.Fatalf("len = %d, want %d", len(b), bucketCount)
	}
	if b[0].Label != "0.0-0.1" || b[9].Label != "0.9-1.0" {
		t.Errorf("labels wrong: first=%q last=%q", b[0].Label, b[9].Label)
	}
}

func TestAggregateHistory(t *testing.T) {
	// Mix of wins/losses across buckets. TotalPnl = RealizedPnl + CashPnl.
	positions := []Position{
		{AvgPrice: 0.05, RealizedPnl: 10, CashPnl: 0},  // bucket 0, win, +10
		{AvgPrice: 0.08, RealizedPnl: 0, CashPnl: -4},  // bucket 0, loss, -4
		{AvgPrice: 0.55, RealizedPnl: 2, CashPnl: 3},   // bucket 5, win, +5
		{AvgPrice: 0.95, RealizedPnl: -1, CashPnl: -1}, // bucket 9, loss, -2
		{AvgPrice: 0.95, RealizedPnl: 0, CashPnl: 0},   // bucket 9, zero -> not a win
	}
	h := aggregateHistory("0xW", positions)

	if h.ResolvedCount != 5 {
		t.Errorf("ResolvedCount = %d, want 5", h.ResolvedCount)
	}
	if h.Wins != 2 {
		t.Errorf("Wins = %d, want 2", h.Wins)
	}
	wantPnl := 10.0 - 4 + 5 - 2 + 0
	if math.Abs(h.TotalRealizedPnl-wantPnl) > 1e-9 {
		t.Errorf("TotalRealizedPnl = %v, want %v", h.TotalRealizedPnl, wantPnl)
	}
	if math.Abs(h.OverallWinRate-2.0/5.0) > 1e-9 {
		t.Errorf("OverallWinRate = %v, want 0.4", h.OverallWinRate)
	}

	// Bucket 0: 2 positions, 1 win, pnl +6.
	b0 := h.Buckets[0]
	if b0.Count != 2 || b0.Wins != 1 || math.Abs(b0.WinRate-0.5) > 1e-9 || math.Abs(b0.TotalPnl-6) > 1e-9 {
		t.Errorf("bucket0 = %+v", b0)
	}
	// Bucket 9: 2 positions, 0 wins (one loss, one zero), pnl -2.
	b9 := h.Buckets[9]
	if b9.Count != 2 || b9.Wins != 0 || b9.WinRate != 0 || math.Abs(b9.TotalPnl+2) > 1e-9 {
		t.Errorf("bucket9 = %+v", b9)
	}
	// Empty bucket stays zeroed.
	if h.Buckets[3].Count != 0 || h.Buckets[3].WinRate != 0 {
		t.Errorf("bucket3 should be empty: %+v", h.Buckets[3])
	}
}

func TestAggregateHistoryEmpty(t *testing.T) {
	h := aggregateHistory("0xW", nil)
	if h.ResolvedCount != 0 || h.Wins != 0 || h.OverallWinRate != 0 || h.TotalRealizedPnl != 0 {
		t.Errorf("empty aggregate non-zero: %+v", h)
	}
	if len(h.Buckets) != bucketCount {
		t.Errorf("buckets len = %d, want %d", len(h.Buckets), bucketCount)
	}
}

func TestUniqueConditionIDs(t *testing.T) {
	positions := []Position{
		{ConditionID: "0xA"},
		{ConditionID: "0xB"},
		{ConditionID: "0xA"}, // dup
		{ConditionID: ""},    // skipped
	}
	got := uniqueConditionIDs(positions)
	if len(got) != 2 {
		t.Fatalf("got %d ids, want 2: %v", len(got), got)
	}
	// Order preserved: A before B.
	if got[0] != "0xA" || got[1] != "0xB" {
		t.Errorf("ids = %v, want [0xA 0xB]", got)
	}
}
