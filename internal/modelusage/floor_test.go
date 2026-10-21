package modelusage

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

// Right after a rollover, utilization moves in ~1-point steps over cents of cost: a rate from that
// would multiply a week of rows into thousands of percent. Every rate waits for minDeltaPct and stays
// under maxPctPerUsd.

func weeklyPctSum(rows []Row) (sum float64, rated bool) {
	for _, row := range rows {
		if row.QuotaWeeklyPct != nil {
			sum += *row.QuotaWeeklyPct
			rated = true
		}
	}
	return sum, rated
}

// rolledOver has $1,000 of last week's rows and windowInput haiku tokens on ia since a rollover 10 minutes ago.
func rolledOver(t *testing.T, windowCost func(*Aggregator)) (*Aggregator, func(used float64) *claudequota.Pool) {
	t.Helper()
	clock := newClock()
	agg := New(clock.Now, 0)
	opus(agg, 250_000_000) // $1,000 last week
	clock.Advance(2 * day)
	resets := clock.Now().Add(7*day - 10*time.Minute)
	windowCost(agg)
	return agg, func(used float64) *claudequota.Pool {
		return &claudequota.Pool{Accounts: 1, ObservedAt: clock.Now(), WeeklyAll: claudequota.Pooled{UsedPct: used, Pooled: 1,
			Windows: []claudequota.AccountWindow{{AuthIndex: "ia", UsedPct: used, ResetsAt: resets, ObservedAt: clock.Now()}}}}
	}
}

func cents(agg *Aggregator) { // $0.01
	agg.HandleUsage(context.Background(), claudeRecord("claude-haiku-5-5", "ia", 100_000, 0, 0, 0))
}

func hundredDollars(agg *Aggregator) { opus(agg, 25_000_000) }

func TestExactWindowNeedsTheFloorAndAPlausibleRate(t *testing.T) {
	for name, tc := range map[string]struct {
		cost    func(*Aggregator)
		used    float64
		present bool
	}{
		"1% over $0.01": {cents, 1, false},
		"5% over $0.01": {cents, 5, false}, // 500 %/USD: no $50 quota spends that fast
		"1% over $100":  {hundredDollars, 1, false},
		"5% over $100":  {hundredDollars, 5, true}, // 0.05 %/USD
	} {
		agg, pool := rolledOver(t, tc.cost)
		snap := agg.SnapshotWithQuota(Range7d, pool(tc.used))
		_, rated := weeklyPctSum(snap.Models)
		if w := snap.Quota.WeeklyAll; (w != nil) != tc.present || rated != tc.present {
			t.Errorf("%s: weekly_all %+v, rows rated %v, want present %v", name, w, rated, tc.present)
			continue
		}
		if !tc.present {
			continue
		}
		if w := snap.Quota.WeeklyAll; !measured(w) || !w.Accounts[0].Exact || !near(w.PctPerUSD, 0.05) {
			t.Errorf("%s: %+v, want the exact measured rate", name, w)
		}
		// Window identity: the rows of the window (the live range covers it) sum to the pooled percent.
		if sum, _ := weeklyPctSum(agg.SnapshotWithQuota(RangeWindow, pool(tc.used)).Models); !near(sum, 5) {
			t.Errorf("%s: window rows sum to %v%%, want 5", name, sum)
		}
	}
}

func TestRoughNeedsTheFloorAndAPlausibleRate(t *testing.T) {
	for name, tc := range map[string]struct {
		input   int64 // opus tokens on ib at $4/MTok
		used    float64
		present bool
	}{
		"1% over $100":  {25_000_000, 1, false},
		"5% over $0.01": {2500, 5, false},
		"5% over $100":  {25_000_000, 5, true},
	} {
		clock := newClock()
		agg := New(clock.Now, 0) // both windows began before tracking: no measured rate
		agg.HandleUsage(context.Background(), claudeRecord("claude-opus-5-5", "ib", tc.input, 0, 0, 0))
		pool := &claudequota.Pool{Accounts: 2, ObservedAt: clock.Now(), WeeklyAll: claudequota.Pooled{UsedPct: tc.used, Pooled: 2,
			Windows: []claudequota.AccountWindow{
				{AuthIndex: "ia", UsedPct: tc.used, ResetsAt: clock.Now().Add(7*day - time.Hour)},
				{AuthIndex: "ib", UsedPct: tc.used, ResetsAt: clock.Now().Add(7*day - 2*time.Hour)},
			}}}
		snap := agg.SnapshotWithQuota(Range7d, pool)
		sum, rated := weeklyPctSum(snap.Models)
		if w := snap.Quota.WeeklyAll; (w != nil) != tc.present || rated != tc.present {
			t.Errorf("%s: weekly_all %+v, want present %v", name, w, tc.present)
			continue
		}
		if tc.present && (snap.Quota.WeeklyAll.Method != MethodRough || !near(sum, 5)) {
			t.Errorf("%s: %+v rows %v%%, want rough summing to 5", name, snap.Quota.WeeklyAll, sum)
		}
	}
}
