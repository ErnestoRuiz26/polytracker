package main

// types.go — Data structures mapping to Polymarket API responses and internal models.
// Field names use Go conventions; JSON tags match the API's snake_case.

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Gamma API: Market discovery
// ---------------------------------------------------------------------------

// Market represents a single prediction market from the Gamma API.
// The ClobTokenIds field is a JSON-encoded string array in the API response
// (e.g. "[\"abc123\",\"def456\"]"), so we unmarshal it manually.
type Market struct {
	ConditionID     string `json:"conditionId"`
	Question        string `json:"question"`
	Slug            string `json:"slug"`
	Active          bool   `json:"active"`
	Closed          bool   `json:"closed"`
	EnableOrderBook bool   `json:"enableOrderBook"`

	// Raw JSON string from the API; parsed into TokenIDs below.
	ClobTokenIdsRaw string `json:"clobTokenIds"`

	// EndDateRaw is the market's resolution date as returned by Gamma (ISO 8601).
	// Parsed on demand via EndTime(); may be empty if the API omits it.
	EndDateRaw string `json:"endDate"`

	// Parsed token IDs for the YES/NO outcome tokens.
	// Populated by ParseTokenIDs() after unmarshaling.
	TokenIDs []string `json:"-"`
}

// EndTime parses EndDateRaw into a time.Time. Returns the zero time if the
// field is empty or unparseable, so callers can treat "unknown" uniformly.
func (m *Market) EndTime() time.Time {
	if m.EndDateRaw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, m.EndDateRaw); err == nil {
		return t
	}
	// Fall back to a date-only layout for APIs that omit the time component.
	if t, err := time.Parse("2006-01-02", m.EndDateRaw); err == nil {
		return t
	}
	return time.Time{}
}

// TrackedMarket pairs a market with the open interest captured at refresh
// time. The poll cycle reuses this OI instead of re-fetching it every cycle,
// so OI is at most one market-refresh-interval stale — acceptable for the
// trade/OI ratio threshold.
type TrackedMarket struct {
	Market  Market
	OI      float64
	EndDate time.Time // resolution date, zero if unknown
}

// ParseTokenIDs decodes the ClobTokenIdsRaw JSON string into TokenIDs.
// Called after unmarshaling because the API returns this as a stringified array.
func (m *Market) ParseTokenIDs() {
	if m.ClobTokenIdsRaw == "" {
		return
	}
	// The raw value is like: "[\"tokenA\",\"tokenB\"]"
	_ = json.Unmarshal([]byte(m.ClobTokenIdsRaw), &m.TokenIDs)
}

// ClobMarket represents a market response from CLOB API's GET /markets/{condition_id}.
type ClobMarket struct {
	ConditionID     string            `json:"condition_id"`
	Question        string            `json:"question"`
	MarketSlug      string            `json:"market_slug"`
	Active          bool              `json:"active"`
	Closed          bool              `json:"closed"`
	EnableOrderBook bool              `json:"enable_order_book"`
	Tokens          []ClobMarketToken `json:"tokens"`
}

type ClobMarketToken struct {
	TokenID string `json:"token_id"`
	Outcome string `json:"outcome"`
}

// ToMarket converts a ClobMarket to the standard Market model.
func (cm *ClobMarket) ToMarket() *Market {
	tokenIDs := make([]string, len(cm.Tokens))
	for i, t := range cm.Tokens {
		tokenIDs[i] = t.TokenID
	}
	return &Market{
		ConditionID:     cm.ConditionID,
		Question:        cm.Question,
		Slug:            cm.MarketSlug,
		Active:          cm.Active,
		Closed:          cm.Closed,
		EnableOrderBook: cm.EnableOrderBook,
		TokenIDs:        tokenIDs,
	}
}

// ---------------------------------------------------------------------------
// Data API: Trades
// ---------------------------------------------------------------------------

// Trade represents a single executed trade from the Data API.
// Size is the number of shares; Price is 0-1 representing probability.
// USD value = Size * Price (since shares are denominated in USDC).
type Trade struct {
	ProxyWallet     string  `json:"proxyWallet"`
	Asset           string  `json:"asset"`
	ConditionID     string  `json:"conditionId"`
	Size            float64 `json:"size"`
	Price           float64 `json:"price"`
	Timestamp       int64   `json:"timestamp"`
	Title           string  `json:"title"`
	Slug            string  `json:"slug"`
	Outcome         string  `json:"outcome"`
	OutcomeIndex    int     `json:"outcomeIndex"`
	Side            string  `json:"side"`
	TransactionHash string  `json:"transactionHash"`
}

// USDValue returns the dollar value of this trade.
// In Polymarket, price is the cost per share in USDC (0-1 range),
// so total cost = shares × price.
func (t *Trade) USDValue() float64 {
	return t.Size * t.Price
}

// ---------------------------------------------------------------------------
// Data API: Open Interest
// ---------------------------------------------------------------------------

// OpenInterest represents OI for a single market condition.
type OpenInterest struct {
	Market string  `json:"market"`
	Value  float64 `json:"value"`
}

// ---------------------------------------------------------------------------
// Data API: Holders
// ---------------------------------------------------------------------------

// Holder represents a single token holder.
type Holder struct {
	ProxyWallet  string  `json:"proxyWallet"`
	Amount       float64 `json:"amount"`
	Name         string  `json:"name"`
	Pseudonym    string  `json:"pseudonym"`
	OutcomeIndex int     `json:"outcomeIndex"`
}

// HolderGroup groups holders by their token (outcome token ID).
type HolderGroup struct {
	Token   string   `json:"token"`
	Holders []Holder `json:"holders"`
}

// ---------------------------------------------------------------------------
// CLOB API: Midpoint
// ---------------------------------------------------------------------------

// MidpointResponse is the CLOB /midpoint response.
type MidpointResponse struct {
	MidPrice string `json:"mid_price"`
}

// ---------------------------------------------------------------------------
// CLOB API: Order Book
// ---------------------------------------------------------------------------

// OrderBookLevel is a single price level in the order book.
type OrderBookLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// OrderBook is the CLOB /book response.
type OrderBook struct {
	Market         string           `json:"market"`
	AssetID        string           `json:"asset_id"`
	Bids           []OrderBookLevel `json:"bids"`
	Asks           []OrderBookLevel `json:"asks"`
	LastTradePrice string           `json:"last_trade_price"`
}

// ---------------------------------------------------------------------------
// Internal: Whale alert
// ---------------------------------------------------------------------------

// WhaleTrade is the enriched alert payload emitted when a trade exceeds
// the OI threshold. Contains all context needed for the consumer to act.
type WhaleTrade struct {
	Alert     string        `json:"alert"`
	Timestamp string        `json:"timestamp"`
	Market    WhaleMarket   `json:"market"`
	Trade     WhaleTradeLeg `json:"trade"`
	Context   WhaleContext  `json:"context"`
}

type WhaleMarket struct {
	Question    string `json:"question"`
	ConditionID string `json:"conditionId"`
	Slug        string `json:"slug"`
	MarketURL   string `json:"marketUrl"`
}

type WhaleTradeLeg struct {
	Size      float64 `json:"size"`
	Price     float64 `json:"price"`
	USDValue  float64 `json:"usdValue"`
	Side      string  `json:"side"`
	Outcome   string  `json:"outcome"`
	Wallet    string  `json:"wallet"`
	TxHash    string  `json:"txHash"`
	Timestamp int64   `json:"timestamp"`
}

type WhaleContext struct {
	OpenInterest         float64        `json:"openInterest"`
	TradeToOIRatio       float64        `json:"tradeToOiRatio"`
	ThresholdPct         float64        `json:"thresholdPct"`
	CurrentMidpoint      float64        `json:"currentMidpoint"`
	PriceRoom            float64        `json:"priceRoom"`
	TimeToResolutionDays float64        `json:"timeToResolutionDays,omitempty"`
	OrderBookDepth       *BookDepthInfo `json:"orderBookDepth,omitempty"`
	WalletIsTopHolder    bool           `json:"walletIsTopHolder"`
	WalletHolderRank     int            `json:"walletHolderRank,omitempty"`
	WalletHolderAmt      float64        `json:"walletHolderAmount,omitempty"`
}

// BookDepthInfo summarizes the order book at alert time.
type BookDepthInfo struct {
	BestBid   string  `json:"bestBid"`
	BestAsk   string  `json:"bestAsk"`
	BidDepth5 float64 `json:"bidDepth5Levels"`
	AskDepth5 float64 `json:"askDepth5Levels"`
}
