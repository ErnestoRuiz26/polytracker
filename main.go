package main

// main.go — Orchestrator for the Polymarket whale trade detector.
// Manages the polling loop, market refresh cycle, and concurrent fan-out
// across markets. Shuts down gracefully on SIGINT/SIGTERM.

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

func printUsage() {
	fmt.Println("Usage: ./polytracker [COMMAND] [FLAGS]")
	fmt.Println("\nCommands:")
	fmt.Println("  help                             Show this help message")
	fmt.Println("  track                            Start tracking all active markets for whale trades")
	fmt.Println("  track --wallet=[WALLET_ID]       Start tracking trade history and new trades for a specific wallet ID")
	fmt.Println("  wallet-history [WALLET_ID]       Show realized P&L and win rate by entry price across a wallet's resolved positions")
	fmt.Println("  compare [WALLET_ID] [WALLET_ID]… Rank wallets by realized P&L / win rate / ROI to decide which are worth copying")
	fmt.Println("\nFlags for 'track':")
	fmt.Println("  --wallet=[WALLET_ID]             (Optional) Specific wallet address to track")
}

func main() {
	banner := " ____       _       _____             _             \n" +
		"|  _ \\ ___ | |_   _|_   _| __ __ _  ___| | _____ _ __ \n" +
		"| |_) / _ \\| | | | | | || '__/ _` |/ __| |/ / _ \\ '__|\n" +
		"|  __/ (_) | | |_| | | || | | (_| | (__|   <  __/ |   \n" +
		"|_|   \\___/|_|\\__, | |_||_|  \\__,_|\\___|_|\\_\\___|_|   \n" +
		"              |___/                                   \n" +
		"  >> Polymarket Whale Tracker\n"
	fmt.Println(banner)

	// Bootstrap a quiet stderr logger so config loading (which happens before
	// setupLogging) doesn't emit INFO noise. setupLogging resets this to the
	// configured level once the config is known.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})))

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		printUsage()
		os.Exit(0)
	}

	var (
		cmdFlag        string
		walletID       string   // track --wallet target
		historyWallet  string   // wallet-history positional target
		compareWallets []string // compare positional targets
	)

	switch cmd {
	case "track":
		trackCmd := flag.NewFlagSet("track", flag.ExitOnError)
		trackCmd.StringVar(&walletID, "wallet", "", "Specific wallet address to track")
		_ = trackCmd.Parse(os.Args[2:])
		cmdFlag = "track"
		if walletID != "" {
			cmdFlag = "track_wallet_" + walletID
		}
	case "wallet-history":
		if len(os.Args) < 3 || strings.HasPrefix(os.Args[2], "-") {
			fmt.Println("Usage: ./polytracker wallet-history <wallet>")
			os.Exit(1)
		}
		historyWallet = os.Args[2]
		cmdFlag = "wallet-history_" + historyWallet
	case "compare":
		if len(os.Args) < 4 {
			fmt.Println("Usage: ./polytracker compare <wallet> <wallet> [wallet…]")
			os.Exit(1)
		}
		for _, a := range os.Args[2:] {
			if strings.HasPrefix(a, "-") {
				fmt.Println("Usage: ./polytracker compare <wallet> <wallet> [wallet…]")
				os.Exit(1)
			}
			compareWallets = append(compareWallets, a)
		}
		cmdFlag = "compare"
	default:
		fmt.Printf("Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}

	// Load config first so its log_level governs operational verbosity. The
	// bootstrap handler above keeps LoadConfig's own messages quiet by default.
	cfg := LoadConfig()

	logFile, err := setupLogging(cmdFlag, cfg.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup logging: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	cfg.PrintSettings()

	client := NewClient(cfg)
	defer client.Close()
	detector := NewDetector(client, cfg)
	notifier := NewDiscordNotifier(cfg.DiscordWebhookURL) // nil when unconfigured
	alerter := NewAlerter(logFile, notifier)

	slog.Info("polytracker starting",
		"command", cmdFlag,
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

	// Drain Discord notifications in the background (nil-safe no-op when off).
	go notifier.Run(ctx)

	// Lifecycle pings: announce start now, and stop on every exit path. The
	// error branches call notifyStop explicitly because os.Exit skips defers.
	startedAt := time.Now()
	host, _ := os.Hostname()
	notifier.NotifyEvent("🟢 Polytracker started",
		fmt.Sprintf("Command: `%s`\nHost: %s", cmdFlag, host), colorStart)
	notifyStop := func(detail string) {
		notifier.NotifyEvent("🔴 Polytracker stopped",
			fmt.Sprintf("Command: `%s`\nHost: %s\nUptime: %s%s",
				cmdFlag, host, time.Since(startedAt).Round(time.Second), detail), colorStop)
	}
	defer notifyStop("")

	switch cmd {
	case "wallet-history":
		if err := RunWalletHistory(ctx, client, cfg, historyWallet); err != nil {
			slog.Error("wallet history failed", "error", err)
			notifyStop("\nReason: error — see logs")
			os.Exit(1)
		}
	case "compare":
		if err := RunWalletCompare(ctx, client, cfg, compareWallets); err != nil {
			slog.Error("wallet compare failed", "error", err)
			notifyStop("\nReason: error — see logs")
			os.Exit(1)
		}
	case "track":
		if walletID != "" {
			runWalletMode(ctx, client, detector, alerter, cfg, walletID)
		} else if err := runMarketMode(ctx, client, detector, alerter, notifier, cfg); err != nil {
			slog.Error("market tracking failed", "error", err)
			notifyStop("\nReason: error — see logs")
			os.Exit(1)
		}
	}
}

// historicalPageSize is how many past trades to fetch per pagination step in
// wallet mode before prompting the user.
const historicalPageSize = 10

// runWalletMode paginates a wallet's historical trades on demand, then switches
// to real-time polling once the user presses [Enter].
func runWalletMode(ctx context.Context, client *Client, detector *Detector, alerter *Alerter, cfg *Config, walletID string) {
	slog.Info("wallet tracking mode active", "wallet", walletID)

	// 1. Fetch and paginate historical trades on startup.
	offset := 0
	var maxTS int64
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Printf("\nFetching historical trades for wallet %s (offset: %d, limit: %d)...\n", walletID, offset, historicalPageSize)
		trades, err := client.FetchUserTrades(ctx, walletID, historicalPageSize, offset)
		if err != nil {
			slog.Error("failed to fetch user trades", "error", err)
			break
		}
		if len(trades) == 0 {
			fmt.Println("No more historical trades found.")
			break
		}

		enriched, err := detector.EnrichTradesDirect(ctx, trades)
		if err != nil {
			slog.Error("failed to enrich user trades", "error", err)
			break
		}

		// Track the newest timestamp to seed the real-time checkpoint.
		for _, t := range trades {
			if t.Timestamp > maxTS {
				maxTS = t.Timestamp
			}
		}

		emitAll(alerter, enriched)

		// Prompt the user for the next batch or to enter real-time tracking.
		fmt.Print("Type 'next' to load the next 10 historical trades, or press [Enter] to start real-time tracking: ")
		if !scanner.Scan() {
			break
		}
		if strings.ToLower(strings.TrimSpace(scanner.Text())) == "next" {
			offset += historicalPageSize
		} else {
			break
		}
	}

	// 2. Transition to real-time tracking.
	fmt.Println("\nStarting real-time tracking...")
	detector.SetWalletCheckpoint(walletID, maxTS)

	pollTicker := time.NewTicker(cfg.PollInterval.Duration)
	defer pollTicker.Stop()

	runWalletPollCycle(ctx, detector, alerter, walletID)
	for {
		select {
		case <-ctx.Done():
			slog.Info("polytracker stopped")
			return
		case <-pollTicker.C:
			runWalletPollCycle(ctx, detector, alerter, walletID)
		}
	}
}

// runMarketMode monitors all active markets, refreshing the tracked-market list
// on its own interval and polling each market for whale trades.
func runMarketMode(ctx context.Context, client *Client, detector *Detector, alerter *Alerter, notifier *DiscordNotifier, cfg *Config) error {
	// Auto-scout: background evaluation of whale wallets into the watchlist.
	// A load failure disables scouting but never blocks tracking.
	var scout *Scout
	if cfg.AutoScout {
		wl, err := LoadWatchlist(cfg.WatchlistFile)
		if err != nil {
			slog.Warn("auto-scout disabled: watchlist unavailable", "error", err)
		} else {
			scout = NewScout(client, cfg, wl, notifier)
			go scout.Run(ctx)
			fmt.Printf("Auto-scout on: %d wallet(s) currently in %s\n", wl.Len(), cfg.WatchlistFile)
		}
	}

	// Bootstrap: fetch markets before entering the main loop. This issues one
	// OI call per market and can take ~10s+, so show progress — operational logs
	// are quiet by default and the wait would otherwise look like a freeze.
	sp := startSpinner("Loading active markets (fetching open interest)…")
	markets, err := refreshMarkets(ctx, client, cfg)
	if err != nil {
		sp.Stop("")
		return fmt.Errorf("initial market fetch: %w", err)
	}
	sp.Stop(fmt.Sprintf("Loaded %d markets. Watching for whale trades…", len(markets)))
	slog.Info("markets loaded", "count", len(markets))

	// Bail immediately if we were cancelled during the (slow) market load.
	if ctx.Err() != nil {
		slog.Info("polytracker stopped (cancelled during startup)")
		return nil
	}

	pollTicker := time.NewTicker(cfg.PollInterval.Duration)
	defer pollTicker.Stop()

	refreshTicker := time.NewTicker(cfg.MarketRefreshInterval.Duration)
	defer refreshTicker.Stop()

	// Run one poll cycle immediately, then enter the tick loop.
	runPollCycle(ctx, detector, alerter, scout, markets, cfg.MaxConcurrency)

	for {
		select {
		case <-ctx.Done():
			slog.Info("polytracker stopped")
			return nil

		case <-refreshTicker.C:
			updated, err := refreshMarkets(ctx, client, cfg)
			if err != nil {
				slog.Warn("market refresh failed, using stale list", "error", err)
			} else {
				markets = updated
				slog.Info("markets refreshed", "count", len(markets))
			}

		case <-pollTicker.C:
			runPollCycle(ctx, detector, alerter, scout, markets, cfg.MaxConcurrency)
		}
	}
}

// refreshMarkets fetches active markets and filters them by the OI window.
// OI lookups are done concurrently (bounded by MaxConcurrency) since there
// can be hundreds of markets and each is an independent HTTP call. The OI is
// carried on each TrackedMarket so the poll cycle does not re-fetch it.
func refreshMarkets(ctx context.Context, client *Client, cfg *Config) ([]TrackedMarket, error) {
	raw, err := client.FetchActiveMarkets(ctx, cfg.MaxMarketsPerCycle)
	if err != nil {
		return nil, err
	}

	slog.Info("fetched raw markets, filtering by OI", "raw_count", len(raw))

	// Concurrently fetch OI for each market.
	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup
	results := make(chan TrackedMarket, len(raw))

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

			results <- TrackedMarket{Market: market, OI: oi, EndDate: market.EndTime()}
		}(m)
	}

	// Close results channel once all goroutines finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	var eligible []TrackedMarket
	for r := range results {
		eligible = append(eligible, r)
	}

	return eligible, nil
}

// runPollCycle fans out CheckMarket calls across all markets using a
// semaphore-bounded goroutine pool. scout may be nil (auto-scout disabled).
func runPollCycle(ctx context.Context, detector *Detector, alerter *Alerter, scout *Scout, markets []TrackedMarket, maxConcurrency int) {
	if len(markets) == 0 {
		return
	}

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var allAlerts []WhaleTrade
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

		go func(market TrackedMarket) {
			defer wg.Done()
			defer func() { <-sem }() // Release slot.

			alerts, err := detector.CheckMarket(ctx, market)
			if err != nil {
				// Suppress "context canceled" noise during shutdown.
				if ctx.Err() != nil {
					return
				}
				slog.Warn("check failed",
					"market", market.Market.Slug,
					"error", err,
				)
				return
			}

			if len(alerts) > 0 {
				mu.Lock()
				allAlerts = append(allAlerts, alerts...)
				mu.Unlock()
			}
		}(m)
	}

	wg.Wait()

	emitAll(alerter, allAlerts)

	// Hand fresh whale wallets to the scout for background evaluation.
	if scout != nil && ctx.Err() == nil {
		scout.Consider(allAlerts)
	}

	// Don't emit summary if we're shutting down — it would be misleading.
	if ctx.Err() == nil {
		alerter.EmitSummary(len(markets), len(allAlerts))
		// Terminal heartbeat: EmitSummary only reaches the log file, so without
		// this the terminal stays silent between alerts and looks frozen.
		fmt.Printf("[%s] checked %d markets, %d alert(s)\n",
			time.Now().Format("15:04:05"), len(markets), len(allAlerts))
	}
}

// runWalletPollCycle polls the detector for a single wallet's new trades
// and emits them newest-first.
func runWalletPollCycle(ctx context.Context, detector *Detector, alerter *Alerter, walletID string) {
	alerts, err := detector.CheckWallet(ctx, walletID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn("wallet check failed", "wallet", walletID, "error", err)
		return
	}
	emitAll(alerter, alerts)
	if ctx.Err() == nil {
		fmt.Printf("[%s] checked wallet %s, %d new trade(s)\n",
			time.Now().Format("15:04:05"), shortID(walletID), len(alerts))
	}
}

// sortAlertsDesc orders alerts by trade execution timestamp, newest first.
func sortAlertsDesc(alerts []WhaleTrade) {
	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].Trade.Timestamp > alerts[j].Trade.Timestamp
	})
}

// emitAll sorts alerts newest-first and emits each one.
func emitAll(alerter *Alerter, alerts []WhaleTrade) {
	sortAlertsDesc(alerts)
	for _, alert := range alerts {
		alerter.EmitAlert(alert)
	}
}

// cleanOldLogs prunes the logs directory:
// 1. Deletes any logs not created/modified today.
// 2. Retains at most maxLogs (including the one about to be created).
func cleanOldLogs(logDir string, maxLogs int) error {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		return fmt.Errorf("read log dir: %w", err)
	}

	var logFiles []os.FileInfo
	now := time.Now()
	todayY, todayM, todayD := now.Date()

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "session_") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Delete files not from today (overwritten/cleaned everyday)
		fY, fM, fD := info.ModTime().Date()
		if fY != todayY || fM != todayM || fD != todayD {
			_ = os.Remove(filepath.Join(logDir, entry.Name()))
			continue
		}

		logFiles = append(logFiles, info)
	}

	// Sort by ModTime (oldest first)
	sort.Slice(logFiles, func(i, j int) bool {
		return logFiles[i].ModTime().Before(logFiles[j].ModTime())
	})

	// Enforce limit (maxLogs - 1 to leave room for the new one)
	limit := maxLogs - 1
	if limit < 0 {
		limit = 0
	}
	for len(logFiles) > limit {
		oldest := logFiles[0]
		_ = os.Remove(filepath.Join(logDir, oldest.Name()))
		logFiles = logFiles[1:]
	}

	return nil
}

// parseLogLevel maps a config string to an slog.Level, defaulting to WARN for
// empty or unrecognized values so the terminal stays quiet by default.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// setupLogging configures human-readable logging on stderr (at the given level)
// and creates the session log file under logs/.
func setupLogging(cmdFlag, logLevel string) (*os.File, error) {
	logDir := "logs"
	maxLogs := 20

	// 1. Clean up old logs
	if err := cleanOldLogs(logDir, maxLogs); err != nil {
		return nil, err
	}

	// 2. Generate new log filename based on session start time
	sessionTime := time.Now().Format("2006-01-02_15-04-05")
	logFileName := fmt.Sprintf("session_%s_%s.log", cmdFlag, sessionTime)
	logFilePath := filepath.Join(logDir, logFileName)

	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	// 3. Configure stderr to use human-readable TextHandler at the chosen level.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(logLevel),
	})))

	slog.Info("logging configured", "log_file", logFilePath)

	return logFile, nil
}
