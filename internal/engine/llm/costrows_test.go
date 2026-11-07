package llm

import (
	"math"
	"testing"
)

// The per-row split of a request's cost: each row is that row's tokens at the
// rate of the tier the request fell in, and the rows add up to Cost.USD.
func TestEstimate_RowsAddUpToTotalAtTheTierInEffect(t *testing.T) {
	card := &PriceCard{
		InputPerMTok: 1, OutputPerMTok: 5, TierTokens: 100, HighInputPerMTok: 2, HighOutputPerMTok: 10,
		CacheReadMult: 0.1, CacheWrite5mMult: 1.25, CacheWrite1hMult: 2,
	}
	cases := []struct {
		name string
		u    Usage
		want CostRows
	}{
		{"low tier", Usage{Input: 100, Output: 1_000_000},
			CostRows{Input: 100e-6, Output: 5}},
		// a 3-token prompt is far below 100: the low rates, cache write at the 5m multiplier.
		{"cache rows at the low rate", Usage{Input: 1, CacheRead: 1, CacheWrite: 1, CacheWrite5m: 1},
			CostRows{Input: 1e-6, CacheRead: 0.1e-6, CacheWrite: 1.25e-6}},
		{"high tier", Usage{Input: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000, CacheWrite5m: 600_000, CacheWrite1h: 400_000, Output: 1_000_000},
			CostRows{Input: 2, CacheRead: 0.2, CacheWrite: 2*1.25*0.6 + 2*2*0.4, Output: 10}},
	}
	for _, c := range cases {
		got := card.Estimate(c.u)
		for _, p := range []struct {
			n         string
			got, want float64
		}{{"input", got.Rows.Input, c.want.Input}, {"cache read", got.Rows.CacheRead, c.want.CacheRead},
			{"cache write", got.Rows.CacheWrite, c.want.CacheWrite}, {"output", got.Rows.Output, c.want.Output}} {
			if math.Abs(p.got-p.want) > 1e-12 {
				t.Errorf("%s: %s = %v, want %v", c.name, p.n, p.got, p.want)
			}
		}
		if sum := got.Rows.Input + got.Rows.CacheRead + got.Rows.CacheWrite + got.Rows.Output; sum != got.USD {
			t.Errorf("%s: rows sum %v != USD %v", c.name, sum, got.USD)
		}
	}
}

func TestEstimate_NilCardHasNoRows(t *testing.T) {
	var c *PriceCard
	if got := c.Estimate(Usage{Input: 5}); got != (Cost{}) {
		t.Errorf("nil card: %+v", got)
	}
}

func TestCost_AddKeepsRows(t *testing.T) {
	a := Cost{USD: 3, Rows: CostRows{Input: 1, Output: 2}}
	b := Cost{USD: 7, Rows: CostRows{Input: 3, CacheRead: 1, Output: 3}}
	got := a.Add(b)
	want := Cost{USD: 10, Rows: CostRows{Input: 4, CacheRead: 1, Output: 5}}
	if got != want {
		t.Errorf("Add = %+v, want %+v", got, want)
	}
}
