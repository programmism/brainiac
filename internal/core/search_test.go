package core

import (
	"testing"

	"github.com/programmism/brainiac/internal/model"
)

func hits(dists ...float64) []model.ChunkHit {
	out := make([]model.ChunkHit, len(dists))
	for i, d := range dists {
		out[i] = model.ChunkHit{Distance: d}
	}
	return out
}

func dists(hs []model.ChunkHit) []float64 {
	out := make([]float64, len(hs))
	for i, h := range hs {
		out[i] = h.Distance
	}
	return out
}

func TestFilterByDistance(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want int // how many survive
	}{
		{"empty", nil, 0},
		{"all within absolute + gap", []float64{0.20, 0.25, 0.30}, 3},
		// best=0.65 so the gap keeps <=0.80; the absolute cutoff (0.75) is what drops
		// 0.80 here, isolating the absolute gate.
		{"absolute cutoff drops the far tail", []float64{0.65, 0.70, 0.80, 0.90}, 2},
		{
			// best=0.20; gap 0.15 → keep <=0.35. 0.30 stays, 0.50 drops even though
			// it's under the absolute 0.75 cutoff — that's the relative calibration.
			"relative gap drops mediocre tail behind a strong best",
			[]float64{0.20, 0.30, 0.50, 0.55},
			2,
		},
		{
			// A weak query: best=0.60, so gap keeps <=0.75, and the absolute cutoff
			// also caps at 0.75 — the 0.72 stays, 0.78 drops.
			"weak query keeps its cluster up to the absolute cap",
			[]float64{0.60, 0.72, 0.78},
			2,
		},
	}
	for _, tc := range cases {
		got := filterByDistance(hits(tc.in...))
		if len(got) != tc.want {
			t.Errorf("%s: kept %v (%d), want %d", tc.name, dists(got), len(got), tc.want)
		}
	}
}
