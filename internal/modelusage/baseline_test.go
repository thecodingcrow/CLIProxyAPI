package modelusage

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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

// opusRead is opus whose response carries ia's weekly reading, as the claude executor records it.
func opusRead(agg *Aggregator, input int64, used float64, resets time.Time) {
	record := claudeRecord("claude-opus-5-5", "ia", input, 0, 0, 0)
	record.ResponseHeaders = http.Header{
		"Anthropic-Ratelimit-Unified-7d-Status":      {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {strconv.FormatFloat(used/100, 'f', -1, 64)},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(resets.Unix(), 10)},
	}
	agg.HandleUsage(context.Background(), record)
}

// Tracking starts mid-window: utilization before it has no cost, so the rate counts only the change since the first reading.
func TestMidWindowTrackingRatesOnlyTheChangeSinceFirstReading(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	resets := clock.Now().Add(6 * day) // window started a day before tracking
	first := clock.Now()
	opusRead(agg, 100_000, 28, resets) // the baseline: its reading pairs with the cost including it, 400,000 µUSD
	if q := agg.SnapshotWithQuota(Range7d, onePool(28, resets, first)).Quota; measured(q.WeeklyAll) || q.Accounts != 2 {
		t.Fatalf("first reading: %+v, want no measured rate yet and 2 pooled accounts", q)
	}
	clock.Advance(time.Hour)
	opusRead(agg, 250_000, 29, resets) // 1,000,000 µUSD; a later reading leaves the baseline
	if q := agg.SnapshotWithQuota(Range7d, onePool(29, resets, clock.Now())).Quota; measured(q.WeeklyAll) {
		t.Fatalf("1-point change: %+v, want no measured rate: noise", q.WeeklyAll)
	}
	q := agg.SnapshotWithQuota(Range7d, onePool(34, resets, clock.Now())).Quota
	w := q.WeeklyAll
	if w == nil || w.CostUSDMicros != 1_000_000 || w.AccountsInRate != 1 || !near(w.PctPerUSD, 6.0/2/1.0) {
		t.Fatalf("6-point change: %+v, want 6%% over 2 pooled accounts per $1", w)
	}
	got := w.Accounts[0]
	if got.Exact || *got.BaselineUsedPct != 28 || !got.BaselineAt.Equal(first) || got.UsedPct != 34 || *got.CostUSDMicros != 1_000_000 {
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
	if w == nil || w.CostUSDMicros != 1_000_000 || !near(w.PctPerUSD, 3.0/2/1.0) || !w.Accounts[0].Exact || *w.Accounts[0].BaselineUsedPct != 0 {
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
	opusRead(agg, 100_000, 90, resets)
	clock.Advance(2 * day) // past the reset
	next := resets.Add(7 * day)
	opusRead(agg, 250_000, 2, next)
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
	opusRead(agg, 100_000, 28, resets) // baseline from the record alone, no read before the flush
	opus(agg, 250_000)
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * day)
	after := New(clock.Now, 0)
	after.Open(path)
	if w := after.SnapshotWithQuota(Range7d, onePool(34, resets, clock.Now())).Quota.WeeklyAll; w == nil || w.CostUSDMicros != 1_000_000 || *w.Accounts[0].BaselineUsedPct != 28 {
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
	opusRead(agg, 100_000, 28, before)
	opus(agg, 250_000)
	w := agg.SnapshotWithQuota(Range7d, onePool(34, before, clock.Now())).Quota.WeeklyAll
	if w == nil || w.Accounts[0].Exact || w.CostUSDMicros != 1_000_000 {
		t.Fatalf("window before tracking: %+v, want inexact, $1 since the first reading", w)
	}
	after := clock.Now().Add(-12*time.Hour + 7*day) // started after the oldest bucket
	if w := agg.SnapshotWithQuota(Range7d, onePool(1, after, clock.Now())).Quota.WeeklyAll; w == nil || !w.Accounts[0].Exact || w.CostUSDMicros != 1_400_000 {
		t.Fatalf("window after tracking start: %+v, want exact", w)
	}
}

// accountState deep-copies the per-account state a read could touch.
func accountState(agg *Aggregator) (map[accountKey]account, bool) {
	agg.mu.Lock()
	defer agg.mu.Unlock()
	out := make(map[accountKey]account, len(agg.accounts))
	for k, acc := range agg.accounts {
		c := account{series: series{buckets: append([]bucket(nil), acc.buckets...), total: acc.total, lastID: acc.lastID}}
		if acc.base != nil {
			b := *acc.base
			c.base = &b
		}
		out[k] = c
	}
	return out, agg.dirty
}

func TestReadNeverMutatesState(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	agg.Open(filepath.Join(t.TempDir(), FileName))
	resets := clock.Now().Add(6 * day)
	opusRead(agg, 100_000, 28, resets)
	opus(agg, 250_000)
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	before, dirty := accountState(agg)
	pool := onePool(40, resets, clock.Now())
	// A window with no baseline, one with an unknown account, and a rolled-over window.
	pool.WeeklyAll.Windows = append(pool.WeeklyAll.Windows,
		claudequota.AccountWindow{AuthIndex: "unseen", UsedPct: 50, ResetsAt: resets, ObservedAt: clock.Now()})
	pool.Fable = claudequota.Pooled{UsedPct: 10, Pooled: 1, Windows: []claudequota.AccountWindow{{AuthIndex: "ia", UsedPct: 10, ResetsAt: resets.Add(-3 * day)}}}
	for _, r := range allRanges {
		agg.SnapshotWithQuota(r, pool)
		agg.SnapshotWithQuota(r, onePool(30, resets.Add(7*day), clock.Now()))
	}
	after, dirtyAfter := accountState(agg)
	if !reflect.DeepEqual(before, after) || dirty || dirtyAfter {
		t.Fatalf("read changed state:\nbefore %+v (dirty %v)\nafter  %+v (dirty %v)", before, dirty, after, dirtyAfter)
	}
}

// A busy account outside the rate would put its spend on every row at the other accounts' rate.
func TestAccountWithCostOutsideTheRateWithholdsIt(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	resetsA := clock.Now().Add(6 * day)                                                             // window started before tracking: inexact
	resetsB := clock.Now().Add(7*day + time.Hour)                                                   // window starts an hour into tracking: exact
	opusRead(agg, 1000, 28, resetsA)                                                                // A's baseline
	clock.Advance(4 * time.Hour)                                                                    // the baseline request leaves RangeWindow
	opusRead(agg, 10_000_000, 31, resetsA)                                                          // $40 on A, 3 points: not yet in the rate
	agg.HandleUsage(context.Background(), claudeRecord("claude-haiku-5-5", "ib", 100_000, 0, 0, 0)) // $0.01 on B
	pool := func(usedA float64) *claudequota.Pool {
		return &claudequota.Pool{Accounts: 2, ObservedAt: clock.Now(), WeeklyAll: claudequota.Pooled{UsedPct: (usedA + 30) / 2, Pooled: 2,
			Windows: []claudequota.AccountWindow{
				{AuthIndex: "ia", UsedPct: usedA, ResetsAt: resetsA, ObservedAt: clock.Now()},
				{AuthIndex: "ib", UsedPct: 30, ResetsAt: resetsB, ObservedAt: clock.Now()},
			}}}
	}
	snap := agg.SnapshotWithQuota(RangeWindow, pool(31))
	if measured(snap.Quota.WeeklyAll) {
		t.Fatalf("A spent outside the rate: %+v, want no measured rate", snap.Quota.WeeklyAll)
	}

	snap = agg.SnapshotWithQuota(RangeWindow, pool(34))
	w := snap.Quota.WeeklyAll
	if w == nil || w.AccountsInRate != 2 {
		t.Fatalf("A past 5 points: %+v, want both accounts in the rate", w)
	}
	pooledDelta := (34.0 - 28 + 30) / 2
	var sum float64
	for _, row := range snap.Models {
		if row.QuotaWeeklyPct != nil {
			sum += *row.QuotaWeeklyPct
		}
	}
	if sum > pooledDelta+1e-6 {
		t.Fatalf("rows sum to %v%%, more than the pooled change %v%%", sum, pooledDelta)
	}
}

func measured(w *QuotaWindow) bool { return w != nil && w.Method == MethodMeasured }
