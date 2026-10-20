package modelusage

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

// onePool pools two accounts; only ia has a running weekly window.
func onePool(used float64, resets, observed time.Time) *claudequota.Pool {
	return &claudequota.Pool{Accounts: 2, ObservedAt: observed, WeeklyAll: claudequota.Pooled{UsedPct: used / 2, Pooled: 2,
		Windows: []claudequota.AccountWindow{{AuthIndex: "ia", UsedPct: used, ResetsAt: resets, ObservedAt: observed}}}}
}

func opus(agg *Aggregator, input int64) { // input x $4/MTok
	agg.HandleUsage(context.Background(), claudeRecord("claude-opus-5-5", "ia", input, 0, 0, 0))
}

// Tracking starts mid-window: utilization before it has no cost, so the rate counts only the change since the first reading.
func TestMidWindowTrackingRatesOnlyTheChangeSinceFirstReading(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	resets := clock.Now().Add(6 * day) // window started a day before tracking
	opus(agg, 100_000)                 // 400,000 µUSD before the first reading
	first := clock.Now()
	if q := agg.SnapshotWithQuota(Range7d, onePool(28, resets, first)).Quota; q.WeeklyAll != nil || q.Accounts != 2 {
		t.Fatalf("first reading: %+v, want no rate yet and 2 pooled accounts", q)
	}
	clock.Advance(time.Hour)
	opus(agg, 250_000) // 1,000,000 µUSD
	if q := agg.SnapshotWithQuota(Range7d, onePool(29, resets, clock.Now())).Quota; q.WeeklyAll != nil {
		t.Fatalf("1-point change: %+v, want omitted as noise", q.WeeklyAll)
	}
	q := agg.SnapshotWithQuota(Range7d, onePool(34, resets, clock.Now())).Quota
	w := q.WeeklyAll
	if w == nil || w.CostUSDMicros != 1_000_000 || w.AccountsInRate != 1 || !near(w.PctPerUSD, 6.0/2/1.0) {
		t.Fatalf("6-point change: %+v, want 6%% over 2 pooled accounts per $1", w)
	}
	got := w.Accounts[0]
	if got.Exact || got.BaselineUsedPct != 28 || !got.BaselineAt.Equal(first) || got.UsedPct != 34 || got.CostUSDMicros != 1_000_000 {
		t.Fatalf("account %+v, want inexact baseline 28%% at the first reading", got)
	}
}

// Tracking covers the window start: the window's whole utilization pairs with its whole cost.
func TestWindowStartedAfterTrackingIsExact(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	opus(agg, 100_000) // before the window
	clock.Advance(2 * day)
	opus(agg, 250_000)
	resets := clock.Now().Add(6 * day) // window started a day ago
	w := agg.SnapshotWithQuota(Range7d, onePool(3, resets, clock.Now())).Quota.WeeklyAll
	if w == nil || w.CostUSDMicros != 1_000_000 || !near(w.PctPerUSD, 3.0/2/1.0) || !w.Accounts[0].Exact || w.Accounts[0].BaselineUsedPct != 0 {
		t.Fatalf("exact window: %+v", w)
	}
	if !w.Accounts[0].BaselineAt.Equal(resets.Add(-7 * day)) {
		t.Fatalf("baseline_at %v, want the window start", w.Accounts[0].BaselineAt)
	}
}

func TestWindowRolloverRestartsTheBaselineAtTheNewWindowStart(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	resets := clock.Now().Add(day) // started before tracking
	agg.SnapshotWithQuota(Range7d, onePool(90, resets, clock.Now()))
	opus(agg, 100_000)
	clock.Advance(2 * day) // past the reset
	opus(agg, 250_000)
	next := resets.Add(7 * day)
	w := agg.SnapshotWithQuota(Range7d, onePool(2, next, clock.Now())).Quota.WeeklyAll
	if w == nil || w.CostUSDMicros != 1_000_000 || !w.Accounts[0].Exact || !near(w.PctPerUSD, 2.0/2/1.0) {
		t.Fatalf("after rollover: %+v, want exact from the new window start", w)
	}
}

func TestBaselinesAndTrackingStartSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	clock := newClock()
	agg := New(clock.Now, 0)
	agg.Open(path)
	resets := clock.Now().Add(6 * day) // started before tracking
	opus(agg, 100_000)
	agg.SnapshotWithQuota(Range7d, onePool(28, resets, clock.Now()))
	opus(agg, 250_000)
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * day)
	after := New(clock.Now, 0)
	after.Open(path)
	if w := after.SnapshotWithQuota(Range7d, onePool(34, resets, clock.Now())).Quota.WeeklyAll; w == nil || w.CostUSDMicros != 1_000_000 || w.Accounts[0].BaselineUsedPct != 28 {
		t.Fatalf("after restart: %+v, want the persisted baseline", w)
	}
	// A window started after tracking began but before the restart: exact only if the tracking start persisted.
	exactStart := clock.Now().Add(-day)
	opus(after, 250_000)
	if w := after.SnapshotWithQuota(Range7d, onePool(1, exactStart.Add(7*day), clock.Now())).Quota.WeeklyAll; w == nil || !w.Accounts[0].Exact || w.CostUSDMicros != 1_000_000 {
		t.Fatalf("tracking start lost on restart: %+v, want an exact baseline", w)
	}
}

// A priced file from before baselines has neither cost_since nor totals: tracking counts from its oldest
// account bucket, and the buckets are the cumulative cost.
func TestPricedFileWithoutTrackingStartTracksFromOldestBucket(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	clock := newClock()
	id := strconv.FormatInt(clock.Now().Unix()/BucketSeconds, 10)
	file := `{"version":1,"priced":true,"since":"2026-09-01T00:00:00Z","series":[],
	 "accounts":[{"auth_index":"ia","last_id":` + id + `,"buckets":[{"id":` + id + `,"requests":0,"cost_usd_micros":400000}]}]}`
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	clock.Advance(day)
	agg := New(clock.Now, 0)
	agg.Open(path)
	before := clock.Now().Add(5 * day) // window started a day before the oldest bucket
	agg.SnapshotWithQuota(Range7d, onePool(28, before, clock.Now()))
	opus(agg, 250_000)
	w := agg.SnapshotWithQuota(Range7d, onePool(34, before, clock.Now())).Quota.WeeklyAll
	if w == nil || w.Accounts[0].Exact || w.CostUSDMicros != 1_000_000 {
		t.Fatalf("window before tracking: %+v, want inexact, $1 since the first reading", w)
	}
	after := clock.Now().Add(-12*time.Hour + 7*day) // started after the oldest bucket
	if w := agg.SnapshotWithQuota(Range7d, onePool(1, after, clock.Now())).Quota.WeeklyAll; w == nil || !w.Accounts[0].Exact || w.CostUSDMicros != 1_000_000 {
		t.Fatalf("window after tracking start: %+v, want exact", w)
	}
}
