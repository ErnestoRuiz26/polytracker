package main

import (
	"math"
	"testing"
)

func TestProfitIfCorrect(t *testing.T) {
	tests := []struct {
		price  float64
		want   float64
		wantOK bool
	}{
		{0.50, 100, true}, // even odds double your money
		{0.20, 400, true}, // cheap longshot pays 4x profit
		{0.997, 0.3009, true},
		{0, 0, false},
		{1, 0, false},
		{-0.1, 0, false},
	}
	for _, tt := range tests {
		pct, ok := profitIfCorrect(tt.price)
		if ok != tt.wantOK {
			t.Errorf("profitIfCorrect(%v) ok = %v, want %v", tt.price, ok, tt.wantOK)
			continue
		}
		if ok && math.Abs(pct-tt.want) > 0.01 {
			t.Errorf("profitIfCorrect(%v) = %v, want %v", tt.price, pct, tt.want)
		}
	}
}
