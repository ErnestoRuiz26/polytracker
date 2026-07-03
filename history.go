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
	"sort"
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
		h.TotalInvested += p.InvestedUSD()
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
	if h.TotalInvested > 0 {
		h.ROI = h.TotalRealizedPnl / h.TotalInvested
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

// buildWalletHistory fetches all of a wallet's positions, determines which
// markets have resolved, and aggregates the realized-P&L / win-rate summary.
// Returns the summary plus the total (resolved + open) position count.
func buildWalletHistory(ctx context.Context, client *Client, cfg *Config, wallet string) (WalletHistory, int, error) {
	positions, err := client.FetchAllPositions(ctx, wallet)
	if err != nil {
		return WalletHistory{}, 0, fmt.Errorf("fetch positions: %w", err)
	}
	if len(positions) == 0 {
		return aggregateHistory(wallet, nil), 0, nil
	}

	conditionIDs := uniqueConditionIDs(positions)
	closed := resolveClosedMarkets(ctx, client, conditionIDs, cfg.MaxConcurrency)

	var resolved []Position
	for _, p := range positions {
		if closed[p.ConditionID] {
			resolved = append(resolved, p)
		}
	}

	return aggregateHistory(wallet, resolved), len(positions), nil
}

// RunWalletHistory computes a single wallet's history and prints the summary.
func RunWalletHistory(ctx context.Context, client *Client, cfg *Config, wallet string) error {
	slog.Info("computing wallet history", "wallet", wallet)

	sp := startSpinner("Fetching positions and market resolutions…")
	hist, total, err := buildWalletHistory(ctx, client, cfg, wallet)
	sp.Stop("")
	if err != nil {
		return err
	}
	if total == 0 {
		fmt.Printf("No positions found for wallet %s\n", wallet)
		return nil
	}

	printWalletHistory(hist, total)
	return nil
}

// Verdict thresholds for the compare command. A wallet is only worth copying
// when its edge is measured over enough resolved bets to not be noise.
const (
	verdictMinSample  = 20   // resolved positions needed for a confident call
	verdictWinRate    = 0.55 // win rate at/above which a profitable wallet is WATCH
	verdictAvoidsRate = 0.45 // win rate below which an unprofitable wallet is AVOID
)

// walletVerdict labels a wallet based on its resolved track record:
// WATCH = profitable with a strong win rate over a real sample — worth copying.
func walletVerdict(h WalletHistory) string {
	if h.ResolvedCount < verdictMinSample {
		return "LOW SAMPLE"
	}
	profitable := h.TotalRealizedPnl > 0
	switch {
	case profitable && h.OverallWinRate >= verdictWinRate:
		return "WATCH"
	case profitable:
		return "OK"
	case h.OverallWinRate < verdictAvoidsRate:
		return "AVOID"
	default:
		return "SKIP"
	}
}

// RunWalletCompare builds histories for several wallets and prints them as a
// ranked table (by total realized P&L) with a copy-worthiness verdict, so the
// user can decide at a glance which wallets to keep an eye on.
func RunWalletCompare(ctx context.Context, client *Client, cfg *Config, wallets []string) error {
	type row struct {
		hist  WalletHistory
		total int
		err   error
	}
	rows := make([]row, len(wallets))

	for i, w := range wallets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sp := startSpinner(fmt.Sprintf("[%d/%d] Analyzing %s…", i+1, len(wallets), shortID(w)))
		hist, total, err := buildWalletHistory(ctx, client, cfg, w)
		sp.Stop("")
		if err != nil {
			slog.Warn("wallet analysis failed", "wallet", shortID(w), "error", err)
		}
		rows[i] = row{hist: hist, total: total, err: err}
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].hist.TotalRealizedPnl > rows[j].hist.TotalRealizedPnl
	})

	fmt.Printf("\n════════════════════════════════════════════════════════════════════════════════\n")
	fmt.Printf(" Wallet comparison — ranked by realized P&L (resolved positions only)\n")
	fmt.Printf("════════════════════════════════════════════════════════════════════════════════\n")
	fmt.Printf(" %-20s %9s %6s %14s %8s  %s\n", "Wallet", "Resolved", "Win%", "P&L", "ROI", "Verdict")
	fmt.Printf("────────────────────────────────────────────────────────────────────────────────\n")
	for _, r := range rows {
		if r.err != nil {
			fmt.Printf(" %-20s %s\n", shortID(r.hist.Wallet), "error: "+r.err.Error())
			continue
		}
		roi := "-"
		if r.hist.TotalInvested > 0 {
			roi = fmt.Sprintf("%+.1f%%", r.hist.ROI*100)
		}
		fmt.Printf(" %-20s %9d %5.0f%% %14s %8s  %s\n",
			shortID(r.hist.Wallet),
			r.hist.ResolvedCount,
			r.hist.OverallWinRate*100,
			fmt.Sprintf("$%+.2f", r.hist.TotalRealizedPnl),
			roi,
			walletVerdict(r.hist))
	}
	fmt.Printf("────────────────────────────────────────────────────────────────────────────────\n")
	fmt.Printf(" WATCH = profitable, win rate ≥ %.0f%%, ≥ %d resolved bets — worth copying.\n",
		verdictWinRate*100, verdictMinSample)
	fmt.Printf(" Track one live: ./polytracker track --wallet=<address>\n")
	fmt.Printf("════════════════════════════════════════════════════════════════════════════════\n\n")
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
	if h.TotalInvested > 0 {
		fmt.Printf(" Est. total invested:   $%.2f  (ROI %+.1f%%)\n", h.TotalInvested, h.ROI*100)
	}
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
