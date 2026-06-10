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

// Detector evaluates trades against OI thresholds and enriches flagged trades.
type Detector struct {
	client *Client
	config *Config

	// seen tracks the most recent trade timestamp we've processed per market.
	// Key: conditionID, Value: unix timestamp of newest trade seen.
	mu   sync.Mutex
	seen map[string]int64
}

// NewDetector creates a Detector with an empty seen-trade cache.
func NewDetector(client *Client, cfg *Config) *Detector {
	return &Detector{
		client: client,
		config: cfg,
		seen:   make(map[string]int64),
	}
}

// CheckMarket inspects recent trades for a single market and returns
// any that exceed the OI threshold, enriched with book/holder context.
func (d *Detector) CheckMarket(ctx context.Context, market Market) ([]WhaleTrade, error) {
	condID := market.ConditionID

	// 1. Fetch recent trades.
	trades, err := d.client.FetchTrades(ctx, condID, 50)
	if err != nil {
		return nil, fmt.Errorf("trades: %w", err)
	}
	if len(trades) == 0 {
		return nil, nil
	}

	// Filter to only trades newer than our last checkpoint.
	d.mu.Lock()
	lastSeen := d.seen[condID]
	d.mu.Unlock()

	var newTrades []Trade
	var maxTS int64
	for _, t := range trades {
		if t.Timestamp > lastSeen {
			newTrades = append(newTrades, t)
		}
		if t.Timestamp > maxTS {
			maxTS = t.Timestamp
		}
	}

	// Update checkpoint even if no whales found — avoids reprocessing.
	if maxTS > lastSeen {
		d.mu.Lock()
		d.seen[condID] = maxTS
		d.mu.Unlock()
	}

	if len(newTrades) == 0 {
		return nil, nil
	}

	// 2. Fetch open interest for this market.
	oi, err := d.client.FetchOpenInterest(ctx, condID)
	if err != nil {
		return nil, fmt.Errorf("OI: %w", err)
	}
	if oi <= 0 {
		slog.Debug("skipping market with zero OI", "conditionId", condID[:16])
		return nil, nil
	}

	// 3. Check each new trade against the threshold.
	var flagged []Trade
	for _, t := range newTrades {
		ratio := t.USDValue() / oi
		if ratio >= d.config.AlertThreshold {
			flagged = append(flagged, t)
		}
	}

	if len(flagged) == 0 {
		return nil, nil
	}

	slog.Info("flagged trades",
		"market", market.Question,
		"count", len(flagged),
		"oi", oi,
	)

	// 4. Enrich each flagged trade with midpoint, book depth, holder status.
	var alerts []WhaleTrade
	for _, t := range flagged {
		alert, err := d.enrichTrade(ctx, market, t, oi)
		if err != nil {
			// Log but don't abort — partial enrichment is better than none.
			slog.Warn("enrichment failed, emitting partial alert",
				"market", condID[:16],
				"error", err,
			)
			alert = d.buildBaseAlert(market, t, oi)
		}
		alerts = append(alerts, alert)
	}

	return alerts, nil
}

// enrichTrade fetches midpoint, order book, and holder data for a flagged trade.
func (d *Detector) enrichTrade(ctx context.Context, market Market, trade Trade, oi float64) (WhaleTrade, error) {
	alert := d.buildBaseAlert(market, trade, oi)

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
	if groups, err := d.client.FetchHolders(ctx, market.ConditionID, 20); err == nil {
		rank, amount, found := findHolder(groups, trade.ProxyWallet)
		alert.Context.WalletIsTopHolder = found
		if found {
			alert.Context.WalletHolderRank = rank
			alert.Context.WalletHolderAmt = amount
		}
	} else {
		slog.Debug("holders fetch failed", "error", err)
	}

	return alert, nil
}

// buildBaseAlert creates the alert struct with data we already have,
// before any enrichment calls.
func (d *Detector) buildBaseAlert(market Market, trade Trade, oi float64) WhaleTrade {
	usd := trade.USDValue()
	return WhaleTrade{
		Alert:     "WHALE_TRADE_DETECTED",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Market: WhaleMarket{
			Question:    market.Question,
			ConditionID: market.ConditionID,
			Slug:        market.Slug,
		},
		Trade: WhaleTradeLeg{
			Size:     trade.Size,
			Price:    trade.Price,
			USDValue: usd,
			Side:     trade.Side,
			Outcome:  trade.Outcome,
			Wallet:   trade.ProxyWallet,
			TxHash:   trade.TransactionHash,
		},
		Context: WhaleContext{
			OpenInterest:   oi,
			TradeToOIRatio: usd / oi,
			ThresholdPct:   d.config.AlertThreshold * 100,
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
