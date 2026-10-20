package modelusage

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

const day = 24 * time.Hour

// quotaFixture records 6 days of traffic on two accounts and returns the clock at the end.
// Account ia: 27% used, window from end-1d; ib: 100% used, window from end-6d; ia's Fable window from end-4d.
func quotaFixture(t *testing.T, previousWeek bool) (*Aggregator, *fakeClock, claudequota.Pool) {
	t.Helper()
	clock := newClock()
	agg := New(clock.Now, 0)
	ctx := context.Background()
	if previousWeek { // ia, before its window started
		agg.HandleUsage(ctx, claudeRecord("claude-opus-5-5", "ia", 1000, 0, 0, 0)) // 4000 µUSD
	}
	clock.Advance(day)
	agg.HandleUsage(ctx, claudeRecord("claude-sonnet-5-5", "ib", 1000, 0, 0, 0)) // 2000
	clock.Advance(4*day + 12*time.Hour)
	agg.HandleUsage(ctx, claudeRecord("claude-opus-5-5", "ia", 2000, 0, 0, 0))  // 8000
	agg.HandleUsage(ctx, claudeRecord("claude-fable-5-1", "ia", 1000, 0, 0, 0)) // 10000
	agg.HandleUsage(ctx, claudeRecord("claude-haiku-5-5", "ib", 1000, 0, 0, 0)) // 100
	agg.HandleUsage(ctx, record("codex", "gpt-5", 10, 10))
	clock.Advance(12 * time.Hour)
	end := clock.Now()
	pool := claudequota.Pool{
		Accounts: 2, ObservedAt: end.Add(-time.Minute),
		WeeklyAll: claudequota.Pooled{UsedPct: 63.5, Pooled: 2, Windows: []claudequota.AccountWindow{
			{AuthIndex: "ia", UsedPct: 27, ResetsAt: end.Add(6 * day)},
			{AuthIndex: "ib", UsedPct: 100, ResetsAt: end.Add(day)},
		}},
		Fable: claudequota.Pooled{UsedPct: 10, Pooled: 1, Windows: []claudequota.AccountWindow{
			{AuthIndex: "ia", UsedPct: 10, ResetsAt: end.Add(3 * day)},
		}},
	}
	return agg, clock, pool
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestRowWeeklyPctOverEachAccountsWindowSumsToPooledPct(t *testing.T) {
	agg, _, pool := quotaFixture(t, false)
	snap := agg.SnapshotWithQuota(Range7d, &pool)

	var weekly, fable float64
	for _, row := range snap.Models {
		switch {
		case row.Provider != "claude":
			if row.QuotaWeeklyPct != nil || row.QuotaFablePct != nil {
				t.Errorf("%s row carries quota: %+v", row.Provider, row)
			}
			continue
		case row.QuotaWeeklyPct == nil:
			t.Fatalf("claude row %s without quota_weekly_pct", row.Model)
		case (row.Model == "claude-fable-5-1") != (row.QuotaFablePct != nil):
			t.Errorf("%s quota_fable_pct = %v", row.Model, row.QuotaFablePct)
		}
		weekly += *row.QuotaWeeklyPct
		if row.QuotaFablePct != nil {
			fable += *row.QuotaFablePct
		}
	}
	if !near(weekly, 63.5) {
		t.Fatalf("sum of quota_weekly_pct %v, want pooled 63.5", weekly)
	}
	if !near(fable, 10) {
		t.Fatalf("sum of quota_fable_pct %v, want pooled Fable 10", fable)
	}
	q := snap.Quota
	if q.WeeklyAll.CostUSDMicros != 20_100 || !near(q.WeeklyAll.PctPerUSD, 63.5/0.0201) || q.WeeklyAll.UsedPct != 63.5 {
		t.Fatalf("weekly_all %+v", q.WeeklyAll)
	}
	if got := q.Fable.Models; len(got) != 1 || got[0] != "claude-fable-5-1" {
		t.Fatalf("fable models %v", got)
	}
}

func TestPreviousWeekSpendStaysOutOfTheAccountsWindow(t *testing.T) {
	agg, clock, pool := quotaFixture(t, true)
	q := agg.SnapshotWithQuota(Range7d, &pool).Quota
	end := clock.Now()
	want := []QuotaAccount{
		{AuthIndex: "ia", UsedPct: 27, WindowStart: end.Add(-day), ResetsAt: end.Add(6 * day), CostUSDMicros: 18_000},
		{AuthIndex: "ib", UsedPct: 100, WindowStart: end.Add(-6 * day), ResetsAt: end.Add(day), CostUSDMicros: 2100},
	}
	if q.WeeklyAll.CostUSDMicros != 20_100 || len(q.WeeklyAll.Accounts) != 2 || q.WeeklyAll.Accounts[0] != want[0] || q.WeeklyAll.Accounts[1] != want[1] {
		t.Fatalf("weekly_all %+v, want accounts %+v", q.WeeklyAll, want)
	}
}

// An account without proxy cost in its window spent only outside the proxy: it leaves the sum,
// yet the pool still divides by every account, so the rate is ia's share of the pool.
func TestAccountWithoutCostInItsWindowIsLeftOut(t *testing.T) {
	agg, _, pool := quotaFixture(t, false)
	pool.WeeklyAll.Windows[1].AuthIndex = "no-traffic"
	q := agg.SnapshotWithQuota(Range7d, &pool).Quota
	if len(q.WeeklyAll.Accounts) != 1 || q.WeeklyAll.CostUSDMicros != 18_000 || !near(q.WeeklyAll.PctPerUSD, 27.0/2/0.018) {
		t.Fatalf("weekly_all %+v, want ia alone at 13.5%% / $0.018", q.WeeklyAll)
	}
}

func TestQuotaOmission(t *testing.T) {
	agg, _, pool := quotaFixture(t, false)
	for name, tc := range map[string]struct {
		pool                      *claudequota.Pool
		block, weekly, fableBlock bool
	}{
		"no quota store":  {pool: nil},
		"no account data": {pool: &claudequota.Pool{}},
		"no running window": {pool: &claudequota.Pool{Accounts: 2, ObservedAt: pool.ObservedAt,
			WeeklyAll: claudequota.Pooled{UsedPct: 10, Pooled: 2}}, block: true},
		"no cost in any window": {pool: &claudequota.Pool{Accounts: 1, ObservedAt: pool.ObservedAt,
			WeeklyAll: claudequota.Pooled{UsedPct: 10, Pooled: 1, Windows: []claudequota.AccountWindow{{AuthIndex: "other", UsedPct: 10, ResetsAt: pool.WeeklyAll.Windows[0].ResetsAt}}}}, block: true},
		"no Fable scope": {pool: &claudequota.Pool{Accounts: 2, ObservedAt: pool.ObservedAt, WeeklyAll: pool.WeeklyAll}, block: true, weekly: true},
	} {
		snap := agg.SnapshotWithQuota(Range7d, tc.pool)
		if (snap.Quota != nil) != tc.block {
			t.Errorf("%s: quota block %+v", name, snap.Quota)
			continue
		}
		if tc.block && ((snap.Quota.WeeklyAll != nil) != tc.weekly || (snap.Quota.Fable != nil) != tc.fableBlock) {
			t.Errorf("%s: weekly_all %+v fable %+v", name, snap.Quota.WeeklyAll, snap.Quota.Fable)
		}
		for _, row := range snap.Models {
			if (row.QuotaWeeklyPct != nil) != (tc.weekly && row.Provider == "claude") || row.QuotaFablePct != nil {
				t.Errorf("%s: row %s/%s weekly %v fable %v", name, row.Provider, row.Model, row.QuotaWeeklyPct, row.QuotaFablePct)
			}
		}
	}
}

func TestAccountCostSurvivesRestart(t *testing.T) {
	agg, clock, pool := quotaFixture(t, true)
	path := filepath.Join(t.TempDir(), FileName)
	agg.Open(path)
	agg.HandleUsage(context.Background(), claudeRecord("claude-haiku-5-5", "ib", 0, 0, 0, 1)) // marks dirty
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	after := New(clock.Now, 0)
	after.Open(path)
	got, want := after.SnapshotWithQuota(Range7d, &pool).Quota, agg.SnapshotWithQuota(Range7d, &pool).Quota
	if got.WeeklyAll.CostUSDMicros != want.WeeklyAll.CostUSDMicros || got.Fable.CostUSDMicros != want.Fable.CostUSDMicros || got.WeeklyAll.CostUSDMicros == 0 {
		t.Fatalf("after restart weekly %+v fable %+v, want weekly %+v fable %+v", got.WeeklyAll, got.Fable, want.WeeklyAll, want.Fable)
	}
}
