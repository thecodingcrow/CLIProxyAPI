package modelusage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

// weekOne fits ia over a whole exact week: 60% for $1,199 (0.05004 %/USD), then rolls over.
// It returns the clock 1 hour into the next window and that window's reset.
func weekOne(t *testing.T) (*Aggregator, *fakeClock, time.Time) {
	t.Helper()
	clock := newClock()
	agg := New(clock.Now, 0)
	resets := clock.Now().Add(7 * day) // the window starts with tracking: exact
	opusRead(agg, 299_750_000, 60, resets)
	clock.Advance(7*day + time.Hour)
	return agg, clock, resets.Add(7 * day)
}

func soloPool(used float64, resets, now time.Time) *claudequota.Pool {
	return &claudequota.Pool{Accounts: 1, ObservedAt: now, WeeklyAll: claudequota.Pooled{UsedPct: used, Pooled: 1,
		Windows: []claudequota.AccountWindow{{AuthIndex: "ia", UsedPct: used, ResetsAt: resets, ObservedAt: now}}}}
}

// rollTo starts the window ending at resets with 1% over $0.01, below every fitting rule.
func rollTo(agg *Aggregator, resets time.Time) {
	readRecord(agg, "claude-haiku-5-5", 100_000, 1, resets)
}

// The issue's acceptance: the plan's rate outlives the week it was measured in.
func TestRolloverCarriesLastWeeksFit(t *testing.T) {
	agg, clock, resets := weekOne(t)
	rollTo(agg, resets)
	before, _ := accountState(agg)
	snap := agg.SnapshotWithQuota(RangeTotal, soloPool(1, resets, clock.Now()))
	if after, _ := accountState(agg); !reflect.DeepEqual(before, after) {
		t.Fatalf("read changed state")
	}
	w := snap.Quota.WeeklyAll
	fit := 60.0 / 1199
	if w == nil || w.Method != MethodCarried || !near(w.PctPerUSD, fit) || w.AccountsInRate != 1 {
		t.Fatalf("weekly_all %+v, want carried at %v %%/USD", w, fit)
	}
	acc := w.Accounts[0]
	if acc.Exact || acc.FitPctPerUSD == nil || !near(*acc.FitPctPerUSD, fit) || acc.FittedAt == nil || acc.BaselineAt != nil {
		t.Fatalf("account %+v, want last week's fit", acc)
	}
	opusRow := findRow(t, snap, "claude", "claude-opus-5-5", false)
	if opusRow.QuotaWeeklyPct == nil || !near(*opusRow.QuotaWeeklyPct, 1199*fit) {
		t.Fatalf("opus row %v, want $1,199 at the carried rate", opusRow.QuotaWeeklyPct)
	}
}

func TestOnlyALargerCostFitReplacesTheStoredOne(t *testing.T) {
	agg, clock, resets := weekOne(t)
	carried := func(resets time.Time) float64 {
		t.Helper()
		w := agg.SnapshotWithQuota(Range7d, soloPool(1, resets, clock.Now())).Quota.WeeklyAll
		if w == nil || w.Method != MethodCarried {
			t.Fatalf("weekly_all %+v, want carried", w)
		}
		return *w.Accounts[0].FitPctPerUSD
	}
	rollTo(agg, resets)
	opusRead(agg, 50_000_000, 20, resets) // week two qualifies at 20 points over $200.01, smaller than $1,199
	clock.Advance(7 * day)
	resets = resets.Add(7 * day)
	rollTo(agg, resets)
	if got := carried(resets); !near(got, 60.0/1199) {
		t.Fatalf("fit %v after a smaller fit, want week one's", got)
	}
	opusRead(agg, 500_000_000, 50, resets) // week three: 50 points over $2,000.01
	clock.Advance(7 * day)
	resets = resets.Add(7 * day)
	rollTo(agg, resets)
	if got := carried(resets); !near(got, 50.0/2000.01) {
		t.Fatalf("fit %v after a larger fit, want week three's", got)
	}
}

func TestFitSurvivesRestartAndRollbackDropsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	agg, clock, resets := weekOne(t)
	agg.Open(path)
	rollTo(agg, resets)
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	after := New(clock.Now, 0)
	after.Open(path)
	if w := after.SnapshotWithQuota(Range7d, soloPool(1, resets, clock.Now())).Quota.WeeklyAll; w == nil || w.Method != MethodCarried {
		t.Fatalf("after restart: %+v, want carried", w)
	}

	// The binary before fits ignores best_fit and drops it on its next flush.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil || state["version"] != 1.0 {
		t.Fatalf("state %v (%v), want version 1", state["version"], err)
	}
	for _, a := range state["accounts"].([]any) {
		delete(a.(map[string]any), "best_fit")
	}
	if data, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	back := New(clock.Now, 0)
	back.Open(path)
	if w := back.SnapshotWithQuota(Range7d, soloPool(1, resets, clock.Now())).Quota.WeeklyAll; w != nil && w.Method == MethodCarried {
		t.Fatalf("after rollback: %+v, want no carried rate", w)
	}
}

// Carried needs a fit for every account that spent in its window.
func TestCarriedNeedsAFitForEverySpendingAccount(t *testing.T) {
	agg, clock, resets := weekOne(t)
	rollTo(agg, resets)
	agg.HandleUsage(t.Context(), claudeRecord("claude-opus-5-5", "ib", 25_000_000, 0, 0, 0)) // $100 on ib, never fitted
	pool := soloPool(1, resets, clock.Now())
	pool.Accounts, pool.WeeklyAll.Pooled = 2, 2
	pool.WeeklyAll.Windows = append(pool.WeeklyAll.Windows, claudequota.AccountWindow{AuthIndex: "ib", UsedPct: 3, ResetsAt: resets.Add(-day)})
	if w := agg.SnapshotWithQuota(Range7d, pool).Quota.WeeklyAll; w != nil && w.Method == MethodCarried {
		t.Fatalf("weekly_all %+v, want no carried rate while ib spent unfitted", w)
	}
}
