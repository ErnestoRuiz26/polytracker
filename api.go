package main

// api.go — HTTP client for all Polymarket API interactions.
// Uses stdlib net/http only. Each method is self-contained with its own
// context-based timeout so callers can cancel individual requests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
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
	shortIDLen     = 16
)

// errNonRetryable wraps responses that should fail immediately (4xx) rather
// than being retried, so getJSON's retry loop can branch on classification
// instead of re-inspecting status codes.
var errNonRetryable = errors.New("non-retryable response")

// shortID truncates an identifier for log/error messages without risking an
// out-of-range slice on short or malformed IDs.
func shortID(s string) string {
	if len(s) <= shortIDLen {
		return s
	}
	return s[:shortIDLen]
}

// Client wraps an HTTP client with Polymarket API base URLs and a rate limiter.
type Client struct {
	http     *http.Client
	gammaURL string
	dataURL  string
	clobURL  string
	ticker   *time.Ticker
	limiter  <-chan time.Time
}

// NewClient creates a Client from the provided config.
func NewClient(cfg *Config) *Client {
	var ticker *time.Ticker
	var limiter <-chan time.Time
	if cfg.RateLimitRPS > 0 {
		interval := time.Second / time.Duration(cfg.RateLimitRPS)
		ticker = time.NewTicker(interval)
		limiter = ticker.C
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
		ticker:   ticker,
		limiter:  limiter,
	}
}

// Close releases the rate-limiter ticker. Safe to call when no limiter is set.
func (c *Client) Close() {
	if c.ticker != nil {
		c.ticker.Stop()
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
		return nil, fmt.Errorf("fetch trades for %s: %w", shortID(conditionID), err)
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
		return 0, fmt.Errorf("fetch OI for %s: %w", shortID(conditionID), err)
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
		return nil, fmt.Errorf("fetch holders for %s: %w", shortID(conditionID), err)
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
		return nil, fmt.Errorf("fetch trades for user %s: %w", shortID(walletAddress), err)
	}
	return trades, nil
}

// FetchMarketByConditionID retrieves a single market from the CLOB API by its condition ID.
func (c *Client) FetchMarketByConditionID(ctx context.Context, conditionID string) (*Market, error) {
	endpoint := fmt.Sprintf("%s/markets/%s", c.clobURL, conditionID)

	var cm ClobMarket
	if err := c.getJSON(ctx, endpoint, &cm); err != nil {
		return nil, fmt.Errorf("fetch clob market by condition ID %s: %w", shortID(conditionID), err)
	}

	return cm.ToMarket(), nil
}

// ---------------------------------------------------------------------------
// HTTP helper with retry
// ---------------------------------------------------------------------------

// getJSON performs a GET request, decodes the JSON response into dest,
// and retries transient failures with exponential backoff. Non-retryable
// outcomes (4xx, decode errors) are wrapped with errNonRetryable and returned
// immediately.
func (c *Client) getJSON(ctx context.Context, rawURL string, dest any) error {
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

		err := c.doRequest(ctx, rawURL, dest)
		if err == nil {
			return nil
		}
		// Stop immediately on non-retryable responses and cancellation.
		if errors.Is(err, errNonRetryable) || ctx.Err() != nil {
			return err
		}
		lastErr = err
	}

	return fmt.Errorf("exhausted retries for %s: %w", rawURL, lastErr)
}

// doRequest performs a single GET attempt with its own per-request timeout
// and decodes a 200 response into dest.
func (c *Client) doRequest(ctx context.Context, rawURL string, dest any) error {
	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// DNS resolution failures are permanent for this host — retrying with
		// backoff just wastes time and floods logs during an outage. Classify
		// them as non-retryable so getJSON fails fast. Connection refused /
		// timeouts are left retryable (server may be restarting).
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) {
			return fmt.Errorf("http do: %w: %w", err, errNonRetryable)
		}
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	// Retry on server errors and rate limits.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}
	// Other non-200s are client errors — fail fast.
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s: %s: %w",
			resp.StatusCode, rawURL, string(body[:min(len(body), 200)]), errNonRetryable)
	}

	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("decode JSON from %s: %w: %w", rawURL, err, errNonRetryable)
	}
	return nil
}
