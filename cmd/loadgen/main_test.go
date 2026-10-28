package main

import (
	"math"
	"testing"
)

func TestVerdict(t *testing.T) {
	tests := []struct {
		name       string
		byStatus   map[int]int
		sent       map[string]int
		transports int
		wantErr    bool
	}{
		{"small n, all as asked", map[int]int{200: 9, 500: 1}, map[string]int{"normal": 9, "failing": 1}, 0, false},
		{"no failing request sent", map[int]int{200: 10}, map[string]int{"normal": 10}, 0, false},
		{"no failing sent: nothing required", map[int]int{200: 5, 503: 5}, map[string]int{"normal": 10}, 0, false},
		{"one 5xx per failing", map[int]int{200: 950, 500: 50}, map[string]int{"normal": 950, "failing": 50}, 0, false},
		{"a few extra 5xx tolerated", map[int]int{200: 947, 500: 53}, map[string]int{"normal": 950, "failing": 50}, 0, false},
		{"demo faults off", map[int]int{200: 1000}, map[string]int{"normal": 950, "failing": 50}, 0, true},
		{"missing 5xx", map[int]int{200: 960, 500: 40}, map[string]int{"normal": 950, "failing": 50}, 0, true},
		{"too many 5xx", map[int]int{200: 900, 500: 100}, map[string]int{"normal": 950, "failing": 50}, 0, true},
		{"transport errors", map[int]int{200: 9}, map[string]int{"normal": 10}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verdict(tt.byStatus, tt.sent, tt.transports)
			if (err != nil) != tt.wantErr {
				t.Errorf("verdict() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckFlags(t *testing.T) {
	tests := []struct {
		name          string
		n, c          int
		errRate, slow float64
		wantErr       bool
	}{
		{"defaults", 1000, 8, 0.05, 0.05, false},
		{"zero requests", 0, 1, 0, 0, false},
		{"no worker", 10, 0, 0.05, 0.05, true},
		{"negative n", -1, 1, 0.05, 0.05, true},
		{"NaN errors", 10, 1, math.NaN(), 0.05, true},
		{"NaN slow", 10, 1, 0.05, math.NaN(), true},
		{"sum above 1", 10, 1, 0.6, 0.6, true},
		{"negative rate", 10, 1, -0.1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkFlags(tt.n, tt.c, tt.errRate, tt.slow)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkFlags() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
