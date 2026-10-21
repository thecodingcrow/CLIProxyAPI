package modelusage

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

// roughFixture is the live case of 2026-10-21: tracking began after both windows started, so no account
// qualifies for a measured rate. Claude spend since the earlier window start is $1,199.
func roughFixture(t *testing.T) (*Aggregator, *claudequota.Pool) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)}
	agg := New(clock.Now, 0)
	ctx := context.Background()
	agg.HandleUsage(ctx, claudeRecord("claude-opus-5-5", "ia", 274_750_000, 0, 0, 0))  // $1,099
	agg.HandleUsage(ctx, claudeRecord("claude-sonnet-5-5", "ia", 50_000_000, 0, 0, 0)) // $100
	agg.HandleUsage(ctx, record("codex", "gpt-5", 10, 10))
	clock.Advance(time.Hour)
	startA, startB := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	return agg, &claudequota.Pool{Accounts: 2, ObservedAt: clock.Now(), WeeklyAll: claudequota.Pooled{UsedPct: 64.5, Pooled: 2,
		Windows: []claudequota.AccountWindow{
			{AuthIndex: "ia", UsedPct: 29, ResetsAt: startA.Add(7 * day), ObservedAt: clock.Now()},
			{AuthIndex: "ib", UsedPct: 100, ResetsAt: startB.Add(7 * day), ObservedAt: clock.Now()},
		}}}
}

func TestRoughRateWhenNoMeasuredRateQualifies(t *testing.T) {
	agg, pool := roughFixture(t)
	snap := agg.SnapshotWithQuota(Range7d, pool)
	w := snap.Quota.WeeklyAll
	if w == nil || w.Method != MethodRough || w.CostUSDMicros != 1_199_000_000 || w.AccountsInRate != 2 || w.UsedPct != 64.5 {
		t.Fatalf("weekly_all %+v, want rough over $1,199 and 2 accounts", w)
	}
	if !near(w.PctPerUSD, 64.5/1199) || math.Abs(w.PctPerUSD-0.054) > 0.001 {
		t.Fatalf("pct_per_usd %v, want ≈0.054", w.PctPerUSD)
	}
	if w.WindowStart == nil || !w.WindowStart.Equal(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("window_start %v, want the earliest account window start", w.WindowStart)
	}
	for _, acc := range w.Accounts {
		if acc.Exact || acc.BaselineAt != nil || acc.BaselineUsedPct != nil || acc.CostUSDMicros != nil {
			t.Fatalf("rough account %+v carries measured fields", acc)
		}
	}
	// All claude spend lies after window_start, so the rows' share sums to the pooled percent.
	var sum float64
	for _, row := range snap.Models {
		if row.Provider == "claude" {
			sum += *row.QuotaWeeklyPct
		} else if row.QuotaWeeklyPct != nil {
			t.Fatalf("%s row rated", row.Provider)
		}
	}
	if math.Abs(sum-64.5) > 0.01 {
		t.Fatalf("rows sum to %v%%, want 64.5", sum)
	}
}

func TestRoughRateOmission(t *testing.T) {
	agg, pool := roughFixture(t)
	zero := *pool
	zero.WeeklyAll.UsedPct = 0
	if w := agg.SnapshotWithQuota(Range7d, &zero).Quota.WeeklyAll; w != nil {
		t.Fatalf("0%% used: %+v, want omitted", w)
	}
	none := *pool
	none.WeeklyAll.Windows = nil
	if w := agg.SnapshotWithQuota(Range7d, &none).Quota.WeeklyAll; w != nil {
		t.Fatalf("no running window: %+v, want omitted", w)
	}
	if w := New(time.Now, 0).SnapshotWithQuota(Range7d, pool).Quota.WeeklyAll; w != nil {
		t.Fatalf("no claude cost: %+v, want omitted", w)
	}
}

func TestMeasuredRateSaysSo(t *testing.T) {
	agg, _, pool := quotaFixture(t, false)
	q := agg.SnapshotWithQuota(Range7d, &pool).Quota
	if q.WeeklyAll.Method != MethodMeasured || q.Fable.Method != MethodMeasured || q.WeeklyAll.WindowStart != nil {
		t.Fatalf("weekly %+v fable %+v, want measured", q.WeeklyAll, q.Fable)
	}
}

func TestRoughFableRateCountsOnlyFableRows(t *testing.T) {
	agg, pool := roughFixture(t)
	agg.HandleUsage(context.Background(), claudeRecord("claude-fable-5-1", "ia", 1_000_000, 0, 0, 0)) // $10
	pool.Fable = claudequota.Pooled{UsedPct: 20, Pooled: 1, Windows: []claudequota.AccountWindow{{AuthIndex: "ia", UsedPct: 20, ResetsAt: pool.WeeklyAll.Windows[1].ResetsAt}}}
	snap := agg.SnapshotWithQuota(Range7d, pool)
	f := snap.Quota.Fable
	if f == nil || f.Method != MethodRough || f.CostUSDMicros != 10_000_000 || !near(f.PctPerUSD, 2) || len(f.Models) != 1 {
		t.Fatalf("fable %+v, want rough 20%% over $10 of Fable rows", f)
	}
	if w := snap.Quota.WeeklyAll; w.CostUSDMicros != 1_209_000_000 {
		t.Fatalf("weekly_all cost %d, want every claude row", w.CostUSDMicros)
	}
}
