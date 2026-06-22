package main

// history.go — `wallet-history <wallet>` command: realized P&L across all of a
// wallet's resolved positions, plus win rate bucketed by entry price.
//
// A position is "resolved" when its market is closed (CLOB markets/{conditionId}
// reports closed=true). A "win" is a resolved position with positive total P&L
// (realized sells + settled value of shares held to resolution).

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// bucketCount is the number of entry-price buckets (deciles: 0.0-0.1 … 0.9-1.0).
const bucketCount = 10

// bucketIndex maps an entry price to its decile bucket [0, bucketCount). Prices
// are clamped, so 1.0 lands in the top bucket and out-of-range values are pinned
// to the ends rather than panicking.
func bucketIndex(price float64) int {
	idx := int(price * bucketCount)
	if idx < 0 {
		return 0
	}
	if idx >= bucketCount {
		return bucketCount - 1
	}
	return idx
}

// newBuckets builds the empty decile buckets with their labels and bounds.
func newBuckets() []PriceBucket {
	buckets := make([]PriceBucket, bucketCount)
	for i := range buckets {
		lo := float64(i) / bucketCount
		hi := float64(i+1) / bucketCount
		buckets[i] = PriceBucket{
			Label: fmt.Sprintf("%.1f-%.1f", lo, hi),
			Lo:    lo,
			Hi:    hi,
		}
	}
	return buckets
}

// aggregateHistory tallies realized P&L and win rate over the supplied resolved
// positions. It is pure — the caller is responsible for filtering positions down
// to resolved markets first. Win = TotalPnl > 0.
func aggregateHistory(wallet string, resolved []Position) WalletHistory {
	buckets := newBuckets()
	h := WalletHistory{Wallet: wallet, ResolvedCount: len(resolved)}

	for i := range resolved {
		p := resolved[i]
		pnl := p.TotalPnl()
		win := pnl > 0

		h.TotalRealizedPnl += pnl
		if win {
			h.Wins++
		}

		b := &buckets[bucketIndex(p.AvgPrice)]
		b.Count++
		b.TotalPnl += pnl
		if win {
			b.Wins++
		}
	}

	for i := range buckets {
		if buckets[i].Count > 0 {
			buckets[i].WinRate = float64(buckets[i].Wins) / float64(buckets[i].Count)
		}
	}
	if h.ResolvedCount > 0 {
		h.OverallWinRate = float64(h.Wins) / float64(h.ResolvedCount)
	}
	h.Buckets = buckets
	return h
}

// resolveClosedMarkets looks up each unique condition ID concurrently and
// returns the set that report closed=true. Lookup failures are logged and
// treated as not-resolved (the position is excluded) rather than aborting.
func resolveClosedMarkets(ctx context.Context, client *Client, conditionIDs []string, maxConcurrency int) map[string]bool {
	closed := make(map[string]bool, len(conditionIDs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrency)

	for _, cid := range conditionIDs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(conditionID string) {
			defer wg.Done()
			defer func() { <-sem }()

			m, err := client.FetchMarketByConditionID(ctx, conditionID)
			if err != nil {
				slog.Debug("resolution lookup failed, treating as unresolved",
					"conditionId", shortID(conditionID), "error", err)
				return
			}
			if m.Closed {
				mu.Lock()
				closed[conditionID] = true
				mu.Unlock()
			}
		}(cid)
	}

	wg.Wait()
	return closed
}

// uniqueConditionIDs returns the distinct, non-empty condition IDs in positions.
func uniqueConditionIDs(positions []Position) []string {
	seen := make(map[string]struct{}, len(positions))
	var ids []string
	for _, p := range positions {
		if p.ConditionID == "" {
			continue
		}
		if _, ok := seen[p.ConditionID]; ok {
			continue
		}
		seen[p.ConditionID] = struct{}{}
		ids = append(ids, p.ConditionID)
	}
	return ids
}

// RunWalletHistory fetches all of a wallet's positions, determines which markets
// have resolved, aggregates the realized-P&L / win-rate summary, and prints it.
func RunWalletHistory(ctx context.Context, client *Client, cfg *Config, wallet string) error {
	slog.Info("computing wallet history", "wallet", wallet)

	positions, err := client.FetchAllPositions(ctx, wallet)
	if err != nil {
		return fmt.Errorf("fetch positions: %w", err)
	}
	if len(positions) == 0 {
		fmt.Printf("No positions found for wallet %s\n", wallet)
		return nil
	}

	closed := resolveClosedMarkets(ctx, client, uniqueConditionIDs(positions), cfg.MaxConcurrency)

	var resolved []Position
	for _, p := range positions {
		if closed[p.ConditionID] {
			resolved = append(resolved, p)
		}
	}

	hist := aggregateHistory(wallet, resolved)
	printWalletHistory(hist, len(positions))
	return nil
}

// printWalletHistory renders the wallet history summary to stdout.
func printWalletHistory(h WalletHistory, totalPositions int) {
	fmt.Printf("\n══════════════════════════════════════════════════════════════\n")
	fmt.Printf(" Wallet history: %s\n", h.Wallet)
	fmt.Printf("══════════════════════════════════════════════════════════════\n")
	fmt.Printf(" Positions scanned:     %d\n", totalPositions)
	fmt.Printf(" Resolved positions:    %d\n", h.ResolvedCount)
	if h.ResolvedCount == 0 {
		fmt.Printf(" (no resolved positions yet — nothing to summarize)\n")
		fmt.Printf("══════════════════════════════════════════════════════════════\n\n")
		return
	}
	fmt.Printf(" Total realized P&L:    $%+.2f\n", h.TotalRealizedPnl)
	fmt.Printf(" Overall win rate:      %.1f%% (%d/%d)\n",
		h.OverallWinRate*100, h.Wins, h.ResolvedCount)
	fmt.Printf("──────────────────────────────────────────────────────────────\n")
	fmt.Printf(" Win rate by entry price (win = position P&L > 0)\n")
	fmt.Printf("──────────────────────────────────────────────────────────────\n")
	fmt.Printf(" %-9s %6s %8s %14s\n", "Price", "N", "Win%", "P&L")
	for _, b := range h.Buckets {
		if b.Count == 0 {
			continue
		}
		fmt.Printf(" %-9s %6d %7.0f%% %13s\n",
			b.Label, b.Count, b.WinRate*100, fmt.Sprintf("$%+.2f", b.TotalPnl))
	}
	fmt.Printf("══════════════════════════════════════════════════════════════\n\n")
}
