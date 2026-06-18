package main

// detector.go — Core anomaly detection logic.
// Stateful only in tracking the last-seen trade timestamp per market
// to avoid duplicate alerts across polling cycles.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// recentTradesLimit is how many recent trades to pull per market/wallet poll.
	recentTradesLimit = 50
	// topHoldersLimit is how many holders to inspect when ranking a wallet.
	topHoldersLimit = 20
)

// Detector evaluates trades against OI thresholds and enriches flagged trades.
type Detector struct {
	client *Client
	config *Config

	// seenMarket / seenWallet track the most recent trade timestamp processed,
	// keyed by conditionID and wallet address respectively. Kept in separate
	// maps so the two keyspaces can never collide.
	mu         sync.Mutex
	seenMarket map[string]int64
	seenWallet map[string]int64
}

// NewDetector creates a Detector with empty seen-trade caches.
func NewDetector(client *Client, cfg *Config) *Detector {
	return &Detector{
		client:     client,
		config:     cfg,
		seenMarket: make(map[string]int64),
		seenWallet: make(map[string]int64),
	}
}

// newTradesSince returns the trades newer than the given checkpoint timestamp
// along with the newest timestamp observed across all supplied trades. It does
// not mutate any state — callers advance the checkpoint only after the trades
// are successfully processed.
func newTradesSince(trades []Trade, lastSeen int64) (newer []Trade, maxTS int64) {
	for _, t := range trades {
		if t.Timestamp > lastSeen {
			newer = append(newer, t)
		}
		if t.Timestamp > maxTS {
			maxTS = t.Timestamp
		}
	}
	return newer, maxTS
}

// Position-action classifications for WhaleContext.PositionAction.
const (
	actionOpen     = "OPEN"
	actionIncrease = "INCREASE"
	actionReduce   = "REDUCE"
	actionClose    = "CLOSE"
	actionUnknown  = "UNKNOWN" // positions lookup failed or trade side unrecognized
)

// classifyPosition decides whether a trade opened, added to, reduced, or closed
// the wallet's position in a token. sizeNow is the wallet's current holding of
// that token (0 if absent from /positions). It infers the pre-trade balance by
// backing out the trade from the current size, with a tolerance to absorb
// fee/rounding drift and minor races between the trade and the snapshot.
func classifyPosition(side string, tradeSize, sizeNow float64) string {
	tol := tradeSize * 0.01
	if tol < 1 {
		tol = 1
	}
	switch strings.ToUpper(side) {
	case "BUY":
		if sizeNow-tradeSize <= tol {
			return actionOpen
		}
		return actionIncrease
	case "SELL":
		if sizeNow <= tol {
			return actionClose
		}
		return actionReduce
	default:
		return actionUnknown
	}
}

// priceRoom returns how far a trade's price sits from certain resolution (1.0).
// A 0.92 entry yields 0.08 of room; clamped to [0,1] for malformed prices.
func priceRoom(price float64) float64 {
	room := 1 - price
	if room < 0 {
		return 0
	}
	if room > 1 {
		return 1
	}
	return room
}

// daysUntil returns the number of days from now until end. Returns 0 when end
// is unknown (zero time); negative values mean the market is already past its
// resolution date.
func daysUntil(end, now time.Time) float64 {
	if end.IsZero() {
		return 0
	}
	return end.Sub(now).Hours() / 24
}

// passesQuickFilters applies the optional signal-quality hard ceilings. Each
// filter is disabled when its config value is 0, so the default behaviour is
// "annotate only, drop nothing".
func passesQuickFilters(cfg *Config, usd, price float64, end, now time.Time) bool {
	if cfg.MinTradeUSD > 0 && usd < cfg.MinTradeUSD {
		return false
	}
	if cfg.MaxSignalPrice > 0 && price > cfg.MaxSignalPrice {
		return false
	}
	if cfg.MinTimeToResolution.Duration > 0 && !end.IsZero() {
		if end.Sub(now) < cfg.MinTimeToResolution.Duration {
			return false
		}
	}
	return true
}

// actionScore maps a position action to its sub-score contribution. Fresh
// conviction (opening / adding) scores high; exiting scores low; unknown sits
// neutral so a failed positions lookup neither rewards nor punishes the trade.
func actionScore(action string) float64 {
	switch action {
	case actionOpen:
		return 1.0
	case actionIncrease:
		return 0.9
	case actionReduce:
		return 0.3
	case actionClose:
		return 0.1
	default: // actionUnknown
		return 0.5
	}
}

// clamp01 bounds x to [0,1].
func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// computeScore combines the annotated sub-signals into a 0-100 composite
// signal score plus the per-signal breakdown. Each sub-signal is normalized to
// [0,1], then blended by the configured (auto-normalized) weights:
//
//   - size:   trade/OI ratio, saturating at cfg.ScoreRefRatio
//   - room:   price room to resolution (1 - price)
//   - time:   days to resolution, saturating at cfg.ScoreRefDays;
//     unknown date (days == 0) is treated as neutral 0.5
//   - action: position action (open/increase/reduce/close)
//
// Returns (0, nil) when total weight is non-positive, so a misconfiguration
// can't silently scale all alerts to the same number.
func computeScore(cfg *Config, ratio, room, days float64, action string) (float64, map[string]float64) {
	w := cfg.ScoreWeights
	total := w.Size + w.Room + w.Time + w.Action
	if total <= 0 {
		return 0, nil
	}

	sSize := clamp01(ratio / cfg.ScoreRefRatio)
	sRoom := clamp01(room)
	var sTime float64
	switch {
	case days == 0: // unknown resolution date
		sTime = 0.5
	case days < 0: // already past resolution
		sTime = 0
	default:
		sTime = clamp01(days / cfg.ScoreRefDays)
	}
	sAction := actionScore(action)

	breakdown := map[string]float64{
		"size":   sSize,
		"room":   sRoom,
		"time":   sTime,
		"action": sAction,
	}

	weighted := w.Size*sSize + w.Room*sRoom + w.Time*sTime + w.Action*sAction
	return 100 * weighted / total, breakdown
}

// scoreAlert computes and attaches the composite signal score to an alert from
// its already-populated context fields. Called after enrichment so PositionAction
// reflects the real classification.
func (d *Detector) scoreAlert(alert *WhaleTrade) {
	c := &alert.Context
	c.SignalScore, c.ScoreBreakdown = computeScore(
		d.config, c.TradeToOIRatio, c.PriceRoom, c.TimeToResolutionDays, c.PositionAction,
	)
}

// CheckMarket inspects recent trades for a single market and returns
// any that exceed the OI threshold, enriched with book/holder context.
// The open interest captured at market-refresh time is reused here rather
// than re-fetched every cycle.
func (d *Detector) CheckMarket(ctx context.Context, tm TrackedMarket) ([]WhaleTrade, error) {
	market := tm.Market
	condID := market.ConditionID
	oi := tm.OI

	if oi <= 0 {
		slog.Debug("skipping market with zero OI", "conditionId", shortID(condID))
		return nil, nil
	}

	// 1. Fetch recent trades.
	trades, err := d.client.FetchTrades(ctx, condID, recentTradesLimit)
	if err != nil {
		return nil, fmt.Errorf("trades: %w", err)
	}
	if len(trades) == 0 {
		return nil, nil
	}

	// Filter to only trades newer than our last checkpoint.
	d.mu.Lock()
	lastSeen := d.seenMarket[condID]
	d.mu.Unlock()

	newTrades, maxTS := newTradesSince(trades, lastSeen)

	// Advance checkpoint only after we've decided we can fully process this
	// market (trades fetched, OI known). Doing it here — rather than before the
	// OI fetch — means a transient failure leaves trades for the next cycle.
	if maxTS > lastSeen {
		d.mu.Lock()
		d.seenMarket[condID] = maxTS
		d.mu.Unlock()
	}

	if len(newTrades) == 0 {
		return nil, nil
	}

	// 2. Check each new trade against the threshold, then the optional
	// signal-quality filters (all no-ops unless configured).
	now := time.Now()
	var flagged []Trade
	for _, t := range newTrades {
		if t.USDValue()/oi < d.config.AlertThreshold {
			continue
		}
		if !passesQuickFilters(d.config, t.USDValue(), t.Price, tm.EndDate, now) {
			continue
		}
		flagged = append(flagged, t)
	}

	if len(flagged) == 0 {
		return nil, nil
	}

	slog.Info("flagged trades",
		"market", market.Question,
		"count", len(flagged),
		"oi", oi,
	)

	// 3. Enrich each flagged trade with midpoint, book depth, holder status,
	// then score it. The composite score gate (min_score) is applied here —
	// after enrichment — because the score depends on PositionAction, which is
	// only known once the positions lookup completes.
	var alerts []WhaleTrade
	for _, t := range flagged {
		alert, err := d.enrichTrade(ctx, market, t, oi, tm.EndDate)
		if err != nil {
			// Log but don't abort — partial enrichment is better than none.
			slog.Warn("enrichment failed, emitting partial alert",
				"market", shortID(condID),
				"error", err,
			)
			alert = d.buildBaseAlert(market, t, oi, tm.EndDate)
			d.scoreAlert(&alert)
		}
		if d.config.MinScore > 0 && alert.Context.SignalScore < d.config.MinScore {
			slog.Debug("dropping trade below min score",
				"score", alert.Context.SignalScore,
				"min", d.config.MinScore,
			)
			continue
		}
		alerts = append(alerts, alert)
	}

	return alerts, nil
}

// enrichTrade fetches midpoint, order book, and holder data for a flagged trade.
func (d *Detector) enrichTrade(ctx context.Context, market Market, trade Trade, oi float64, endDate time.Time) (WhaleTrade, error) {
	alert := d.buildBaseAlert(market, trade, oi, endDate)

	// Determine which token ID to use for CLOB calls.
	// The trade's Asset field is the token ID.
	tokenID := trade.Asset
	if tokenID == "" && len(market.TokenIDs) > 0 {
		// Fallback: use the first token ID from the market.
		tokenID = market.TokenIDs[0]
	}

	if tokenID == "" {
		return alert, fmt.Errorf("no token ID available for CLOB enrichment")
	}

	// Fetch midpoint (best-effort).
	if mid, err := d.client.FetchMidpoint(ctx, tokenID); err == nil {
		alert.Context.CurrentMidpoint = mid
	} else {
		slog.Debug("midpoint fetch failed", "error", err)
	}

	// Fetch order book and compute depth summary (best-effort).
	if book, err := d.client.FetchOrderBook(ctx, tokenID); err == nil {
		alert.Context.OrderBookDepth = summarizeBook(book)
	} else {
		slog.Debug("book fetch failed", "error", err)
	}

	// Check if the wallet is a top holder (best-effort).
	if groups, err := d.client.FetchHolders(ctx, market.ConditionID, topHoldersLimit); err == nil {
		rank, amount, found := findHolder(groups, trade.ProxyWallet)
		alert.Context.WalletIsTopHolder = found
		if found {
			alert.Context.WalletHolderRank = rank
			alert.Context.WalletHolderAmt = amount
		}
	} else {
		slog.Debug("holders fetch failed", "error", err)
	}

	// Classify open vs close from the wallet's current position (best-effort).
	// On failure the alert keeps the UNKNOWN default set in buildBaseAlert.
	if positions, err := d.client.FetchPositions(ctx, trade.ProxyWallet, market.ConditionID); err == nil {
		var sizeNow float64
		var matched *Position
		for i := range positions {
			if positions[i].Asset == trade.Asset {
				matched = &positions[i]
				sizeNow = positions[i].Size
				break
			}
		}
		alert.Context.PositionAction = classifyPosition(trade.Side, trade.Size, sizeNow)
		if matched != nil {
			alert.Context.WalletAvgPrice = matched.AvgPrice
			alert.Context.WalletRealizedPnl = matched.RealizedPnl
			alert.Context.WalletPositionSize = matched.Size
		}
	} else {
		slog.Debug("positions fetch failed", "error", err)
	}

	// Score the alert from its now-populated context (ratio, room, time, action).
	d.scoreAlert(&alert)

	return alert, nil
}

// buildBaseAlert creates the alert struct with data we already have,
// before any enrichment calls.
func (d *Detector) buildBaseAlert(market Market, trade Trade, oi float64, endDate time.Time) WhaleTrade {
	usd := trade.USDValue()
	ttr := daysUntil(endDate, time.Now())
	return WhaleTrade{
		Alert:     "WHALE_TRADE_DETECTED",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Market: WhaleMarket{
			Question:    market.Question,
			ConditionID: market.ConditionID,
			Slug:        market.Slug,
			MarketURL:   "https://polymarket.com/market/" + market.Slug,
		},
		Trade: WhaleTradeLeg{
			Size:      trade.Size,
			Price:     trade.Price,
			USDValue:  usd,
			Side:      trade.Side,
			Outcome:   trade.Outcome,
			Wallet:    trade.ProxyWallet,
			TxHash:    trade.TransactionHash,
			Timestamp: trade.Timestamp,
		},
		Context: WhaleContext{
			OpenInterest:         oi,
			TradeToOIRatio:       usd / oi,
			ThresholdPct:         d.config.AlertThreshold * 100,
			PriceRoom:            priceRoom(trade.Price),
			TimeToResolutionDays: ttr,
			PositionAction:       actionUnknown,
		},
	}
}

// summarizeBook computes a depth summary from the top 5 bid/ask levels.
func summarizeBook(book *OrderBook) *BookDepthInfo {
	info := &BookDepthInfo{}

	if len(book.Bids) > 0 {
		info.BestBid = book.Bids[0].Price
	}
	if len(book.Asks) > 0 {
		info.BestAsk = book.Asks[0].Price
	}

	// Sum the top 5 levels of liquidity (price × size) on each side.
	info.BidDepth5 = sumDepth(book.Bids, 5)
	info.AskDepth5 = sumDepth(book.Asks, 5)

	return info
}

// sumDepth totals price*size for up to n levels.
func sumDepth(levels []OrderBookLevel, n int) float64 {
	var total float64
	for i, lvl := range levels {
		if i >= n {
			break
		}
		price, _ := strconv.ParseFloat(lvl.Price, 64)
		size, _ := strconv.ParseFloat(lvl.Size, 64)
		total += price * size
	}
	return total
}

// findHolder checks whether a wallet address appears in any holder group.
// Returns the 1-based rank, amount held, and whether the wallet was found.
func findHolder(groups []HolderGroup, wallet string) (rank int, amount float64, found bool) {
	wallet = strings.ToLower(wallet)
	for _, g := range groups {
		for i, h := range g.Holders {
			if strings.ToLower(h.ProxyWallet) == wallet {
				return i + 1, h.Amount, true
			}
		}
	}
	return 0, 0, false
}

// EnrichTradesDirect fetches market metadata and Open Interest, constructs the WhaleTrade objects,
// and performs full CLOB/holder enrichment.
func (d *Detector) EnrichTradesDirect(ctx context.Context, trades []Trade) ([]WhaleTrade, error) {
	var alerts []WhaleTrade
	for _, t := range trades {
		if t.ConditionID == "" {
			continue
		}

		// 1. Fetch market metadata.
		m, err := d.client.FetchMarketByConditionID(ctx, t.ConditionID)
		if err != nil {
			slog.Warn("skipping enrichment: failed to fetch market metadata", "conditionID", t.ConditionID, "error", err)
			// Construct a basic fallback market
			m = &Market{
				ConditionID: t.ConditionID,
				Question:    t.Title,
				Slug:        t.Slug,
			}
			if m.Question == "" {
				m.Question = "Polymarket Bet"
			}
			if m.Slug == "" {
				m.Slug = "polymarket-bet"
			}
		}

		// 2. Fetch Open Interest.
		oi, err := d.client.FetchOpenInterest(ctx, t.ConditionID)
		if err != nil {
			slog.Debug("failed to fetch OI, using fallback of 1", "conditionID", t.ConditionID, "error", err)
			oi = 1.0 // fallback so we don't divide by zero
		}

		// 3. Build base alert and enrich it. The CLOB market lookup here does
		// not carry a resolution date, so endDate is zero (ttr omitted).
		alert, err := d.enrichTrade(ctx, *m, t, oi, m.EndTime())
		if err != nil {
			slog.Warn("enrichment failed, using partial alert", "error", err)
			alert = d.buildBaseAlert(*m, t, oi, m.EndTime())
			d.scoreAlert(&alert)
		}
		alerts = append(alerts, alert)
	}
	return alerts, nil
}

// SetWalletCheckpoint seeds the last-seen timestamp for a wallet so that
// historical trades already shown are not re-emitted when real-time tracking
// begins.
func (d *Detector) SetWalletCheckpoint(walletAddress string, ts int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ts > d.seenWallet[walletAddress] {
		d.seenWallet[walletAddress] = ts
	}
}

// CheckWallet inspects recent trades for a user wallet and returns
// new trades since the last check, enriched with context.
func (d *Detector) CheckWallet(ctx context.Context, walletAddress string) ([]WhaleTrade, error) {
	trades, err := d.client.FetchUserTrades(ctx, walletAddress, recentTradesLimit, 0)
	if err != nil {
		return nil, fmt.Errorf("user trades: %w", err)
	}
	if len(trades) == 0 {
		return nil, nil
	}

	// Filter to only trades newer than our last checkpoint.
	d.mu.Lock()
	lastSeen := d.seenWallet[walletAddress]
	d.mu.Unlock()

	newTrades, maxTS := newTradesSince(trades, lastSeen)

	if maxTS > lastSeen {
		d.mu.Lock()
		d.seenWallet[walletAddress] = maxTS
		d.mu.Unlock()
	}

	if len(newTrades) == 0 {
		return nil, nil
	}

	// Enrich the new trades.
	return d.EnrichTradesDirect(ctx, newTrades)
}
