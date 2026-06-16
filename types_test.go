package main

import (
	"math"
	"reflect"
	"testing"
)

func TestTradeUSDValue(t *testing.T) {
	tests := []struct {
		name  string
		trade Trade
		want  float64
	}{
		{"whole", Trade{Size: 100, Price: 0.5}, 50},
		{"zero price", Trade{Size: 100, Price: 0}, 0},
		{"fractional", Trade{Size: 1234.5, Price: 0.65}, 802.425},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.trade.USDValue(); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("USDValue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseTokenIDs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"valid", `["abc123","def456"]`, []string{"abc123", "def456"}},
		{"empty string", "", nil},
		{"malformed leaves nil", `["abc",`, nil},
		{"empty array", `[]`, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Market{ClobTokenIdsRaw: tt.raw}
			m.ParseTokenIDs()
			if !reflect.DeepEqual(m.TokenIDs, tt.want) {
				t.Errorf("TokenIDs = %#v, want %#v", m.TokenIDs, tt.want)
			}
		})
	}
}

func TestClobMarketToMarket(t *testing.T) {
	cm := ClobMarket{
		ConditionID:     "0xcond",
		Question:        "Will X?",
		MarketSlug:      "will-x",
		Active:          true,
		Closed:          false,
		EnableOrderBook: true,
		Tokens: []ClobMarketToken{
			{TokenID: "tokA", Outcome: "Yes"},
			{TokenID: "tokB", Outcome: "No"},
		},
	}
	m := cm.ToMarket()
	if m.ConditionID != "0xcond" || m.Question != "Will X?" || m.Slug != "will-x" {
		t.Errorf("scalar fields mismatch: %+v", m)
	}
	if !m.Active || m.Closed || !m.EnableOrderBook {
		t.Errorf("bool fields mismatch: %+v", m)
	}
	if !reflect.DeepEqual(m.TokenIDs, []string{"tokA", "tokB"}) {
		t.Errorf("TokenIDs = %#v", m.TokenIDs)
	}
}
