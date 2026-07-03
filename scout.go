package main

// scout.go — Auto-scout mode. Every whale wallet that trips an alert in market
// mode is evaluated in the background through the same resolved-history
// pipeline as the `compare` command, and wallets earning a WATCH verdict are
// persisted to a local watchlist file. The tracker finds copy-worthy whales by
// itself; the user reviews watchlist.json and picks whom to follow live with
// `track --wallet=…`.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// WatchlistEntry is one scouted wallet in the watchlist file.
type WatchlistEntry struct {
	Wallet      string  `json:"wallet"`
	Verdict     string  `json:"verdict"`
	RealizedPnl float64 `json:"realizedPnl"`
	WinRate     float64 `json:"winRate"` // 0-1 over resolved positions
	ROI         float64 `json:"roi"`     // 0 when invested unknown
	Resolved    int     `json:"resolvedPositions"`
	TriggeredBy string  `json:"triggeredBy"` // market question of the alert that first surfaced the wallet
	AddedAt     string  `json:"addedAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// watchlistFile is the on-disk shape of the watchlist.
type watchlistFile struct {
	Wallets map[string]WatchlistEntry `json:"wallets"`
}

// Watchlist is a mutex-guarded set of scouted wallets persisted to a JSON file,
// keyed by lowercase wallet address.
type Watchlist struct {
	mu      sync.Mutex
	path    string
	wallets map[string]WatchlistEntry
}

// LoadWatchlist reads the watchlist at path, creating an empty file when none
// exists so the user always has a watchlist.json to inspect.
func LoadWatchlist(path string) (*Watchlist, error) {
	wl := &Watchlist{path: path, wallets: make(map[string]WatchlistEntry)}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := wl.save(); err != nil {
			return nil, fmt.Errorf("create watchlist: %w", err)
		}
		return wl, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read watchlist: %w", err)
	}

	var f watchlistFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse watchlist %s: %w", path, err)
	}
	if f.Wallets != nil {
		wl.wallets = f.Wallets
	}
	return wl, nil
}

// save writes the watchlist to disk. Callers must hold mu (or have exclusive
// access, as during LoadWatchlist).
func (wl *Watchlist) save() error {
	data, err := json.MarshalIndent(watchlistFile{Wallets: wl.wallets}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(wl.path, append(data, '\n'), 0644)
}

// Len returns the number of wallets currently on the watchlist.
func (wl *Watchlist) Len() int {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	return len(wl.wallets)
}

// Get returns the entry for a wallet and whether it exists.
func (wl *Watchlist) Get(wallet string) (WatchlistEntry, bool) {
	wl.mu.Lock()
	defer wl.mu.Unlock()
	e, ok := wl.wallets[strings.ToLower(wallet)]
	return e, ok
}

// Upsert adds or refreshes a wallet entry and persists the file. AddedAt (and
// the original TriggeredBy) are preserved across updates. Returns whether the
// wallet is new to the list.
func (wl *Watchlist) Upsert(e WatchlistEntry) (isNew bool, err error) {
	key := strings.ToLower(e.Wallet)
	e.Wallet = key
	now := time.Now().UTC().Format(time.RFC3339)

	wl.mu.Lock()
	defer wl.mu.Unlock()
	prev, ok := wl.wallets[key]
	if ok {
		e.AddedAt = prev.AddedAt
		if prev.TriggeredBy != "" {
			e.TriggeredBy = prev.TriggeredBy
		}
	} else {
		e.AddedAt = now
	}
	e.UpdatedAt = now
	wl.wallets[key] = e
	return !ok, wl.save()
}

const (
	// scoutQueueSize bounds the pending-evaluation backlog. Overflow is
	// dropped — the wallet re-triggers on its next whale trade.
	scoutQueueSize = 64
	// rescoutAfter is how long before an already-evaluated wallet is
	// re-scouted when it trips another alert.
	rescoutAfter = 24 * time.Hour
)

// scoutJob is one wallet queued for evaluation.
type scoutJob struct {
	wallet      string
	triggeredBy string
}

// Scout evaluates whale wallets in the background and maintains the watchlist.
type Scout struct {
	client   *Client
	cfg      *Config
	wl       *Watchlist
	notifier *DiscordNotifier // nil when Discord notifications are off

	mu      sync.Mutex
	scouted map[string]time.Time // last evaluation attempt per wallet (session-scoped)
	queue   chan scoutJob
}

// NewScout creates a Scout writing to the given watchlist. notifier may be nil.
func NewScout(client *Client, cfg *Config, wl *Watchlist, notifier *DiscordNotifier) *Scout {
	return &Scout{
		client:   client,
		cfg:      cfg,
		wl:       wl,
		notifier: notifier,
		scouted:  make(map[string]time.Time),
		queue:    make(chan scoutJob, scoutQueueSize),
	}
}

// Run processes scout jobs until ctx is cancelled. Jobs are handled serially:
// a full history build for an active wallet can mean thousands of rate-limited
// API calls, so one at a time keeps scouting from starving the alert poll loop.
func (s *Scout) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-s.queue:
			s.evaluate(ctx, job)
		}
	}
}

// Consider queues the wallets behind fresh alerts for evaluation. The cheap
// track record already fetched during enrichment acts as a prefilter, so
// obviously unprofitable whales cost no extra API calls. Note the prefilter
// includes unrealized P&L, so a wallet must look strong on that blended view
// before the (expensive) resolved-only evaluation runs.
func (s *Scout) Consider(alerts []WhaleTrade) {
	for _, a := range alerts {
		ws := a.Context.WalletStats
		if ws == nil || ws.TotalPnl <= 0 || ws.WinRate < verdictWinRate || ws.Decided < verdictMinSample {
			continue
		}
		wallet := strings.ToLower(a.Trade.Wallet)
		if wallet == "" || !s.markScouted(wallet) {
			continue
		}
		select {
		case s.queue <- scoutJob{wallet: a.Trade.Wallet, triggeredBy: a.Market.Question}:
		default:
			// Queue full: unmark so the wallet is retried on its next alert.
			s.unmarkScouted(wallet)
			slog.Debug("scout queue full, dropping", "wallet", shortID(wallet))
		}
	}
}

// markScouted reports whether the wallet is due for evaluation and, when it is,
// records the attempt so concurrent alerts don't double-queue it.
func (s *Scout) markScouted(wallet string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.scouted[wallet]; ok && time.Since(t) < rescoutAfter {
		return false
	}
	s.scouted[wallet] = time.Now()
	return true
}

// unmarkScouted clears a wallet's evaluation record so it can be re-queued.
func (s *Scout) unmarkScouted(wallet string) {
	s.mu.Lock()
	delete(s.scouted, wallet)
	s.mu.Unlock()
}

// evaluate runs the full resolved-history pipeline on one wallet. WATCH
// verdicts are added to the watchlist; wallets already on the list get their
// stats (and possibly a downgraded verdict) refreshed rather than removed, so
// the user sees a fading wallet instead of it silently disappearing.
func (s *Scout) evaluate(ctx context.Context, job scoutJob) {
	hist, _, err := buildWalletHistory(ctx, s.client, s.cfg, job.wallet)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("scout evaluation failed", "wallet", shortID(job.wallet), "error", err)
		}
		s.unmarkScouted(strings.ToLower(job.wallet))
		return
	}

	verdict := walletVerdict(hist)
	_, onList := s.wl.Get(job.wallet)
	if verdict != verdictWatch && !onList {
		slog.Info("scouted wallet, not watch-worthy",
			"wallet", shortID(job.wallet),
			"verdict", verdict,
			"resolved", hist.ResolvedCount,
			"pnl", hist.TotalRealizedPnl,
		)
		return
	}

	entry := WatchlistEntry{
		Wallet:      job.wallet,
		Verdict:     verdict,
		RealizedPnl: hist.TotalRealizedPnl,
		WinRate:     hist.OverallWinRate,
		ROI:         hist.ROI,
		Resolved:    hist.ResolvedCount,
		TriggeredBy: job.triggeredBy,
	}
	isNew, err := s.wl.Upsert(entry)
	if err != nil {
		slog.Warn("failed to persist watchlist", "error", err)
		return
	}

	if isNew {
		wallet := strings.ToLower(job.wallet)
		fmt.Printf("[scout] added %s to %s — $%+.2f P&L, %.0f%% win rate, %d resolved bets (triggered by: %s)\n",
			wallet, s.cfg.WatchlistFile,
			hist.TotalRealizedPnl, hist.OverallWinRate*100, hist.ResolvedCount, job.triggeredBy)

		roi := "n/a"
		if hist.TotalInvested > 0 {
			roi = fmt.Sprintf("%+.1f%%", hist.ROI*100)
		}
		s.notifier.NotifyEvent("🔭 New WATCH wallet scouted",
			fmt.Sprintf("Wallet: `%s`\nP&L: $%+.2f | Win rate: %.0f%% | ROI: %s | Resolved bets: %d\nTriggered by: %s\nFollow live: `./polytracker track --wallet=%s`",
				wallet, hist.TotalRealizedPnl, hist.OverallWinRate*100, roi, hist.ResolvedCount,
				job.triggeredBy, wallet),
			colorScout)
	} else {
		fmt.Printf("[scout] refreshed %s on %s — verdict %s, $%+.2f P&L, %.0f%% win rate\n",
			shortID(strings.ToLower(job.wallet)), s.cfg.WatchlistFile,
			verdict, hist.TotalRealizedPnl, hist.OverallWinRate*100)
	}
}
