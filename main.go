package main

// main.go — Orchestrator for the Polymarket whale trade detector.
// Manages the polling loop, market refresh cycle, and concurrent fan-out
// across markets. Shuts down gracefully on SIGINT/SIGTERM.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	banner := " ____       _       _____             _             \n" +
		"|  _ \\ ___ | |_   _|_   _| __ __ _  ___| | _____ _ __ \n" +
		"| |_) / _ \\| | | | | | || '__/ _` |/ __| |/ / _ \\ '__|\n" +
		"|  __/ (_) | | |_| | | || | | (_| | (__|   <  __/ |   \n" +
		"|_|   \\___/|_|\\__, | |_||_|  \\__,_|\\___|_|\\_\\___|_|   \n" +
		"              |___/                                   \n" +
		"  >> Polymarket Whale Tracker\n"
	fmt.Println(banner)

	// Operational logs go to stderr as JSON so they don't mix with alerts on stdout.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg := LoadConfig()
	client := NewClient(cfg)
	detector := NewDetector(client, cfg)
	alerter := NewAlerter()

	slog.Info("polytracker starting",
		"threshold_pct", cfg.AlertThreshold*100,
		"poll_interval", cfg.PollInterval.Duration.String(),
		"min_oi", cfg.MinOpenInterest,
		"max_oi", cfg.MaxOpenInterest,
		"max_concurrency", cfg.MaxConcurrency,
	)

	// Graceful shutdown via context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig.String())
		cancel()
	}()

	// Bootstrap: fetch markets before entering the main loop.
	markets, err := refreshMarkets(ctx, client, cfg)
	if err != nil {
		slog.Error("initial market fetch failed", "error", err)
		os.Exit(1)
	}
	slog.Info("markets loaded", "count", len(markets))

	// Bail immediately if we were cancelled during the (slow) market load.
	if ctx.Err() != nil {
		slog.Info("polytracker stopped (cancelled during startup)")
		return
	}

	// Tickers for the two loops.
	pollTicker := time.NewTicker(cfg.PollInterval.Duration)
	defer pollTicker.Stop()

	refreshTicker := time.NewTicker(cfg.MarketRefreshInterval.Duration)
	defer refreshTicker.Stop()

	// Run one poll cycle immediately, then enter the tick loop.
	runPollCycle(ctx, detector, alerter, markets, cfg.MaxConcurrency)

	for {
		select {
		case <-ctx.Done():
			slog.Info("polytracker stopped")
			return

		case <-refreshTicker.C:
			updated, err := refreshMarkets(ctx, client, cfg)
			if err != nil {
				slog.Warn("market refresh failed, using stale list", "error", err)
			} else {
				markets = updated
				slog.Info("markets refreshed", "count", len(markets))
			}

		case <-pollTicker.C:
			runPollCycle(ctx, detector, alerter, markets, cfg.MaxConcurrency)
		}
	}
}

// marketWithOI pairs a market with its pre-fetched OI value.
type marketWithOI struct {
	market Market
	oi     float64
}

// refreshMarkets fetches active markets and filters them by the OI window.
// OI lookups are done concurrently (bounded by MaxConcurrency) since there
// can be hundreds of markets and each is an independent HTTP call.
func refreshMarkets(ctx context.Context, client *Client, cfg *Config) ([]Market, error) {
	raw, err := client.FetchActiveMarkets(ctx, cfg.MaxMarketsPerCycle)
	if err != nil {
		return nil, err
	}

	slog.Info("fetched raw markets, filtering by OI", "raw_count", len(raw))

	// Concurrently fetch OI for each market.
	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup
	results := make(chan marketWithOI, len(raw))

	for _, m := range raw {
		if len(m.TokenIDs) == 0 {
			continue // Can't do CLOB lookups without token IDs.
		}

		// Bail early if context is cancelled.
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{} // Acquire slot.

		go func(market Market) {
			defer wg.Done()
			defer func() { <-sem }()

			oi, err := client.FetchOpenInterest(ctx, market.ConditionID)
			if err != nil {
				slog.Debug("skipping market, OI fetch failed",
					"slug", market.Slug,
					"error", err,
				)
				return
			}

			if oi < cfg.MinOpenInterest {
				return
			}
			if cfg.MaxOpenInterest > 0 && oi > cfg.MaxOpenInterest {
				return
			}

			results <- marketWithOI{market: market, oi: oi}
		}(m)
	}

	// Close results channel once all goroutines finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	var eligible []Market
	for r := range results {
		eligible = append(eligible, r.market)
	}

	return eligible, nil
}

// runPollCycle fans out CheckMarket calls across all markets using a
// semaphore-bounded goroutine pool.
func runPollCycle(ctx context.Context, detector *Detector, alerter *Alerter, markets []Market, maxConcurrency int) {
	if len(markets) == 0 {
		return
	}

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var totalAlerts int
	var mu sync.Mutex

	for _, m := range markets {
		// Check for cancellation before spawning.
		// NOTE: break inside select only breaks the select, not the for loop.
		// We must check ctx.Err() directly.
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{} // Acquire semaphore slot.

		go func(market Market) {
			defer wg.Done()
			defer func() { <-sem }() // Release slot.

			alerts, err := detector.CheckMarket(ctx, market)
			if err != nil {
				// Suppress "context canceled" noise during shutdown.
				if ctx.Err() != nil {
					return
				}
				slog.Warn("check failed",
					"market", market.Slug,
					"error", err,
				)
				return
			}

			for _, alert := range alerts {
				alerter.EmitAlert(alert)
			}

			if len(alerts) > 0 {
				mu.Lock()
				totalAlerts += len(alerts)
				mu.Unlock()
			}
		}(m)
	}

	wg.Wait()

	// Don't emit summary if we're shutting down — it would be misleading.
	if ctx.Err() == nil {
		alerter.EmitSummary(len(markets), totalAlerts)
	}
}
