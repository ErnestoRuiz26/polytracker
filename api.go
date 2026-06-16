package main

// api.go — HTTP client for all Polymarket API interactions.
// Uses stdlib net/http only. Each method is self-contained with its own
// context-based timeout so callers can cancel individual requests.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	httpTimeout    = 10 * time.Second
	maxRetries     = 3
	baseRetryDelay = 500 * time.Millisecond
	userAgent      = "polytracker/1.0"
)

// Client wraps an HTTP client with Polymarket API base URLs and a rate limiter.
type Client struct {
	http     *http.Client
	gammaURL string
	dataURL  string
	clobURL  string
	limiter  <-chan time.Time
}

// NewClient creates a Client from the provided config.
func NewClient(cfg *Config) *Client {
	var limiter <-chan time.Time
	if cfg.RateLimitRPS > 0 {
		interval := time.Second / time.Duration(cfg.RateLimitRPS)
		limiter = time.Tick(interval)
	}

	return &Client{
		http: &http.Client{
			Timeout: httpTimeout,
			// Don't follow redirects — API shouldn't redirect.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		gammaURL: cfg.GammaBaseURL,
		dataURL:  cfg.DataBaseURL,
		clobURL:  cfg.CLOBBaseURL,
		limiter:  limiter,
	}
}

// ---------------------------------------------------------------------------
// Gamma API
// ---------------------------------------------------------------------------

// FetchActiveMarkets retrieves all active, order-book-enabled markets.
// Paginates until exhausted or maxMarkets is reached.
func (c *Client) FetchActiveMarkets(ctx context.Context, maxMarkets int) ([]Market, error) {
	var allMarkets []Market
	limit := 100
	offset := 0

	for {
		params := url.Values{
			"active":          {"true"},
			"closed":          {"false"},
			"enableOrderBook": {"true"},
			"limit":           {strconv.Itoa(limit)},
			"offset":          {strconv.Itoa(offset)},
		}

		endpoint := fmt.Sprintf("%s/markets?%s", c.gammaURL, params.Encode())

		var page []Market
		if err := c.getJSON(ctx, endpoint, &page); err != nil {
			return allMarkets, fmt.Errorf("fetch markets page offset=%d: %w", offset, err)
		}

		for i := range page {
			page[i].ParseTokenIDs()
		}
		allMarkets = append(allMarkets, page...)

		// Stop conditions: empty page, or we've hit the cap.
		if len(page) < limit || len(allMarkets) >= maxMarkets {
			break
		}
		offset += limit
	}

	// Trim to cap if we overshot.
	if len(allMarkets) > maxMarkets {
		allMarkets = allMarkets[:maxMarkets]
	}
	return allMarkets, nil
}

// ---------------------------------------------------------------------------
// Data API
// ---------------------------------------------------------------------------

// FetchTrades retrieves recent trades for a market by condition ID.
func (c *Client) FetchTrades(ctx context.Context, conditionID string, limit int) ([]Trade, error) {
	params := url.Values{
		"market": {conditionID},
		"limit":  {strconv.Itoa(limit)},
	}
	endpoint := fmt.Sprintf("%s/trades?%s", c.dataURL, params.Encode())

	var trades []Trade
	if err := c.getJSON(ctx, endpoint, &trades); err != nil {
		return nil, fmt.Errorf("fetch trades for %s: %w", conditionID[:16], err)
	}
	return trades, nil
}

// FetchOpenInterest returns the OI value (USD) for a single market.
// Returns 0 if the market has no OI data.
func (c *Client) FetchOpenInterest(ctx context.Context, conditionID string) (float64, error) {
	params := url.Values{
		"market": {conditionID},
	}
	endpoint := fmt.Sprintf("%s/oi?%s", c.dataURL, params.Encode())

	var results []OpenInterest
	if err := c.getJSON(ctx, endpoint, &results); err != nil {
		return 0, fmt.Errorf("fetch OI for %s: %w", conditionID[:16], err)
	}

	if len(results) == 0 {
		return 0, nil
	}
	return results[0].Value, nil
}

// FetchHolders returns the top holders for a market.
func (c *Client) FetchHolders(ctx context.Context, conditionID string, limit int) ([]HolderGroup, error) {
	if limit > 20 {
		limit = 20 // API cap
	}
	params := url.Values{
		"market": {conditionID},
		"limit":  {strconv.Itoa(limit)},
	}
	endpoint := fmt.Sprintf("%s/holders?%s", c.dataURL, params.Encode())

	var groups []HolderGroup
	if err := c.getJSON(ctx, endpoint, &groups); err != nil {
		return nil, fmt.Errorf("fetch holders for %s: %w", conditionID[:16], err)
	}
	return groups, nil
}

// ---------------------------------------------------------------------------
// CLOB API
// ---------------------------------------------------------------------------

// FetchMidpoint returns the current midpoint price for a token as a float.
func (c *Client) FetchMidpoint(ctx context.Context, tokenID string) (float64, error) {
	params := url.Values{
		"token_id": {tokenID},
	}
	endpoint := fmt.Sprintf("%s/midpoint?%s", c.clobURL, params.Encode())

	var resp MidpointResponse
	if err := c.getJSON(ctx, endpoint, &resp); err != nil {
		return 0, fmt.Errorf("fetch midpoint: %w", err)
	}

	price, err := strconv.ParseFloat(resp.MidPrice, 64)
	if err != nil {
		return 0, fmt.Errorf("parse midpoint %q: %w", resp.MidPrice, err)
	}
	return price, nil
}

// FetchOrderBook returns the full order book for a token.
func (c *Client) FetchOrderBook(ctx context.Context, tokenID string) (*OrderBook, error) {
	params := url.Values{
		"token_id": {tokenID},
	}
	endpoint := fmt.Sprintf("%s/book?%s", c.clobURL, params.Encode())

	var book OrderBook
	if err := c.getJSON(ctx, endpoint, &book); err != nil {
		return nil, fmt.Errorf("fetch book: %w", err)
	}
	return &book, nil
}

// FetchUserTrades retrieves recent trades for a specific user (wallet address).
func (c *Client) FetchUserTrades(ctx context.Context, walletAddress string, limit, offset int) ([]Trade, error) {
	params := url.Values{
		"user":   {walletAddress},
		"limit":  {strconv.Itoa(limit)},
		"offset": {strconv.Itoa(offset)},
	}
	endpoint := fmt.Sprintf("%s/trades?%s", c.dataURL, params.Encode())

	var trades []Trade
	if err := c.getJSON(ctx, endpoint, &trades); err != nil {
		return nil, fmt.Errorf("fetch trades for user %s: %w", walletAddress[:min(len(walletAddress), 10)], err)
	}
	return trades, nil
}

// FetchMarketByConditionID retrieves a single market from the CLOB API by its condition ID.
func (c *Client) FetchMarketByConditionID(ctx context.Context, conditionID string) (*Market, error) {
	endpoint := fmt.Sprintf("%s/markets/%s", c.clobURL, conditionID)

	var cm ClobMarket
	if err := c.getJSON(ctx, endpoint, &cm); err != nil {
		return nil, fmt.Errorf("fetch clob market by condition ID %s: %w", conditionID[:min(len(conditionID), 10)], err)
	}

	return cm.ToMarket(), nil
}

// ---------------------------------------------------------------------------
// HTTP helper with retry
// ---------------------------------------------------------------------------

// getJSON performs a GET request, decodes the JSON response into dest,
// and retries transient failures with exponential backoff.
func (c *Client) getJSON(ctx context.Context, rawURL string, dest interface{}) error {
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 500ms, 1s, 2s
			delay := time.Duration(float64(baseRetryDelay) * math.Pow(2, float64(attempt-1)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			slog.Debug("retrying request", "url", rawURL, "attempt", attempt)
		}

		if c.limiter != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-c.limiter:
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http do: %w", err)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read body: %w", readErr)
			continue
		}

		// Retry on server errors and rate limits.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, rawURL, string(body[:min(len(body), 200)]))
		}

		if err := json.Unmarshal(body, dest); err != nil {
			return fmt.Errorf("decode JSON from %s: %w", rawURL, err)
		}
		return nil
	}

	return fmt.Errorf("exhausted retries for %s: %w", rawURL, lastErr)
}
