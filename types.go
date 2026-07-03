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
// Data API: Positions
// ---------------------------------------------------------------------------

// Position is a wallet's current holding of a single outcome token, from the
// Data API /positions endpoint. Used to classify whether a trade opened or
// closed a position (see classifyPosition).
type Position struct {
	ProxyWallet string  `json:"proxyWallet"`
	Asset       string  `json:"asset"`
	ConditionID string  `json:"conditionId"`
	Size        float64 `json:"size"`
	AvgPrice    float64 `json:"avgPrice"`
	RealizedPnl float64 `json:"realizedPnl"`
	CashPnl     float64 `json:"cashPnl"`
	CurPrice    float64 `json:"curPrice"`
	Title       string  `json:"title"`

	// InitialValue is the USD cost basis of the shares still held; TotalBought
	// is the cumulative number of shares ever bought (including ones since sold).
	// avgPrice × totalBought approximates the total USD ever put into the
	// position, which is the denominator for ROI.
	InitialValue float64 `json:"initialValue"`
	TotalBought  float64 `json:"totalBought"`
}

// InvestedUSD estimates the total dollars ever committed to this position.
func (p *Position) InvestedUSD() float64 {
	return p.AvgPrice * p.TotalBought
}

// TotalPnl is the position's full P&L: profit locked in from earlier sells
// (RealizedPnl) plus the gain/loss on shares still held (CashPnl). For a market
// that has resolved, the held portion is settled, so this is fully realized.
func (p *Position) TotalPnl() float64 {
	return p.RealizedPnl + p.CashPnl
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

	// PositionAction classifies the trade relative to the wallet's prior holding
	// of the token: OPEN, INCREASE, REDUCE, CLOSE, or UNKNOWN (positions lookup
	// failed). Derived from the /positions snapshot — see classifyPosition.
	PositionAction     string  `json:"positionAction"`
	WalletAvgPrice     float64 `json:"walletAvgPrice,omitempty"`
	WalletRealizedPnl  float64 `json:"walletRealizedPnl,omitempty"`
	WalletPositionSize float64 `json:"walletPositionSize,omitempty"`

	// WalletStats is the trading wallet's overall track record (all positions,
	// realized + unrealized), fetched best-effort and cached. Nil when the
	// lookup failed — the alert still fires without it.
	WalletStats *WalletStatsInfo `json:"walletStats,omitempty"`

	// SignalScore is the composite 0-100 signal strength (see computeScore).
	// ScoreBreakdown holds the individual [0,1] sub-scores for tuning/debug.
	SignalScore    float64            `json:"signalScore"`
	ScoreBreakdown map[string]float64 `json:"scoreBreakdown,omitempty"`
}

// WalletStatsInfo summarizes a wallet's overall Polymarket track record so an
// alert reader can judge whether the whale is worth copying. Derived from the
// full /positions snapshot: P&L counts realized plus unrealized, and a "win"
// is any position with positive total P&L (positions with ~zero P&L — e.g.
// just opened — are excluded from the win-rate sample).
type WalletStatsInfo struct {
	TotalPnl  float64 `json:"totalPnl"`
	WinRate   float64 `json:"winRate"` // 0-1 over decided positions
	Decided   int     `json:"decidedPositions"`
	Positions int     `json:"totalPositions"`
}

// ---------------------------------------------------------------------------
// Internal: Wallet history
// ---------------------------------------------------------------------------

// PriceBucket aggregates resolved positions whose entry price (avgPrice) falls
// in [Lo, Hi). A "win" is a position with positive total P&L.
type PriceBucket struct {
	Label    string  `json:"label"`
	Lo       float64 `json:"lo"`
	Hi       float64 `json:"hi"`
	Count    int     `json:"count"`
	Wins     int     `json:"wins"`
	WinRate  float64 `json:"winRate"` // 0-1, 0 when Count is 0
	TotalPnl float64 `json:"totalPnl"`
}

// WalletHistory is the realized-P&L and win-rate summary for a wallet across
// all of its resolved positions.
type WalletHistory struct {
	Wallet           string        `json:"wallet"`
	ResolvedCount    int           `json:"resolvedPositions"`
	Wins             int           `json:"wins"`
	OverallWinRate   float64       `json:"overallWinRate"` // 0-1
	TotalRealizedPnl float64       `json:"totalRealizedPnl"`
	TotalInvested    float64       `json:"totalInvested"` // sum of estimated USD committed
	ROI              float64       `json:"roi"`           // TotalRealizedPnl / TotalInvested, 0 when invested unknown
	Buckets          []PriceBucket `json:"buckets"`
}

// BookDepthInfo summarizes the order book at alert time.
type BookDepthInfo struct {
	BestBid   string  `json:"bestBid"`
	BestAsk   string  `json:"bestAsk"`
	BidDepth5 float64 `json:"bidDepth5Levels"`
	AskDepth5 float64 `json:"askDepth5Levels"`
}
