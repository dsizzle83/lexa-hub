package main

import (
	"math"
	"testing"
)

func TestPlausibleW(t *testing.T) {
	cases := []struct {
		name string
		w    float64
		maxW float64
		want bool
	}{
		{"half nameplate", 2400, 4800, true},
		{"at nameplate", 4800, 4800, true},
		{"within tolerance", 5600, 4800, true}, // 4800*1.2 = 5760
		{"just over tolerance", 5800, 4800, false},
		{"bad-scale 10x", 48000, 4800, false},
		{"negative within rating (battery charge)", -4000, 4800, true},
		{"negative bad-scale", -48000, 4800, false},
		{"unknown nameplate accepts", 48000, 0, true},
		{"NaN rejected", math.NaN(), 4800, false},
		{"Inf rejected", math.Inf(1), 4800, false},
	}
	for _, c := range cases {
		if got := plausibleW(c.w, c.maxW); got != c.want {
			t.Errorf("%s: plausibleW(%v, %v) = %v, want %v", c.name, c.w, c.maxW, got, c.want)
		}
	}
}
