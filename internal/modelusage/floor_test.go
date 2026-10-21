package modelusage

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

// Right after a rollover, utilization moves in ~1-point steps over cents of cost: a rate from that
// would multiply a week of rows into thousands of percent. Both methods wait for minDeltaPct.

func weeklyPctSum(rows []Row) (sum float64, rated bool) {
	for _, row := range rows {
		if row.QuotaWeeklyPct != nil {
			sum += *row.QuotaWeeklyPct
			rated = true
		}
	}
	return sum, rated
}

func TestExactWindowWaitsForTheFloor(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	opus(agg, 250_000_000) // $1,000 last week
	clock.Advance(2 * day)
	resets := clock.Now().Add(7*day - 10*time.Minute)                                               // rolled over 10 minutes ago
	agg.HandleUsage(context.Background(), claudeRecord("claude-haiku-5-5", "ia", 100_000, 0, 0, 0)) // $0.01
	pool := func(used float64) *claudequota.Pool {
		return &claudequota.Pool{Accounts: 1, ObservedAt: clock.Now(), WeeklyAll: claudequota.Pooled{UsedPct: used, Pooled: 1,
			Windows: []claudequota.AccountWindow{{AuthIndex: "ia", UsedPct: used, ResetsAt: resets, ObservedAt: clock.Now()}}}}
	}
	snap := agg.SnapshotWithQuota(Range7d, pool(1))
	if _, rated := weeklyPctSum(snap.Models); snap.Quota.WeeklyAll != nil || rated {
		t.Fatalf("1%% after rollover: %+v, want no weekly_all and no row share", snap.Quota.WeeklyAll)
	}
	w := agg.SnapshotWithQuota(Range7d, pool(5)).Quota.WeeklyAll
	if !measured(w) || !w.Accounts[0].Exact || !near(w.PctPerUSD, 5/0.01) {
		t.Fatalf("5%%: %+v, want the exact measured rate", w)
	}
	// Window identity: the rows of the window (the live range covers it) sum to the pooled percent.
	if sum, _ := weeklyPctSum(agg.SnapshotWithQuota(RangeWindow, pool(5)).Models); !near(sum, 5) {
		t.Fatalf("window rows sum to %v%%, want 5", sum)
	}
}

func TestRoughWaitsForTheFloor(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0) // both windows began before tracking: no measured rate
	opus(agg, 250_000_000)   // $1,000
	agg.HandleUsage(context.Background(), claudeRecord("claude-haiku-5-5", "ib", 100_000, 0, 0, 0))
	pool := func(used float64) *claudequota.Pool {
		return &claudequota.Pool{Accounts: 2, ObservedAt: clock.Now(), WeeklyAll: claudequota.Pooled{UsedPct: used, Pooled: 2,
			Windows: []claudequota.AccountWindow{
				{AuthIndex: "ia", UsedPct: used, ResetsAt: clock.Now().Add(7*day - time.Hour), ObservedAt: clock.Now()},
				{AuthIndex: "ib", UsedPct: used, ResetsAt: clock.Now().Add(7*day - 2*time.Hour), ObservedAt: clock.Now()},
			}}}
	}
	snap := agg.SnapshotWithQuota(Range7d, pool(1))
	if _, rated := weeklyPctSum(snap.Models); snap.Quota.WeeklyAll != nil || rated {
		t.Fatalf("1%% pooled: %+v, want no weekly_all and no row share", snap.Quota.WeeklyAll)
	}
	snap = agg.SnapshotWithQuota(Range7d, pool(5))
	if w := snap.Quota.WeeklyAll; w == nil || w.Method != MethodRough {
		t.Fatalf("5%% pooled: %+v, want rough", w)
	}
	if sum, _ := weeklyPctSum(snap.Models); !near(sum, 5) {
		t.Fatalf("rows since window_start sum to %v%%, want 5", sum)
	}
}
