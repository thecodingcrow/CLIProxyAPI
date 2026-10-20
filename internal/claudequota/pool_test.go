package claudequota

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestPoolListsEachAccountsRunningWeeklyWindow(t *testing.T) {
	now := observedNow
	resetA, resetB, resetFable := now.Add(6*24*time.Hour), now.Add(24*time.Hour), now.Add(3*24*time.Hour)
	a := withWindow(http.Header{}, "7d", "allowed", "0.27", resetA)
	withWindow(a, "7d_oi", "allowed", "0.10", resetFable)
	b := withWindow(http.Header{}, "7d", "rejected", "1.00", resetB)
	// Read 8 days ago: its window ended, so it pools at 0% with no running window.
	ended := withWindow(http.Header{}, "7d", "allowed", "0.50", now.Add(-24*time.Hour))
	store := newTestStore(now,
		Credential{ID: "a", Index: "ia", Name: "a", Observations: []Observation{observe(t, a, now)}},
		Credential{ID: "b", Index: "ib", Name: "b", Observations: []Observation{observe(t, b, now.Add(-time.Hour))}},
		Credential{ID: "c", Index: "ic", Name: "c", Observations: []Observation{observe(t, ended, now.Add(-8*24*time.Hour))}},
		Credential{ID: "d", Index: "id", Name: "d", Disabled: true, Observations: []Observation{observe(t, a, now)}},
		Credential{ID: "e", Index: "ie", Name: "e"},
	)

	pool := store.Snapshot().Pool()

	want := Pool{
		Accounts:   3,
		ObservedAt: now,
		Stale:      true,
		WeeklyAll: Pooled{UsedPct: 42.33, Pooled: 3, Windows: []AccountWindow{
			{AuthIndex: "ia", UsedPct: 27, ResetsAt: resetA, ObservedAt: now},
			{AuthIndex: "ib", UsedPct: 100, ResetsAt: resetB, ObservedAt: now.Add(-time.Hour)},
		}},
		Fable: Pooled{UsedPct: 10, Pooled: 1, Windows: []AccountWindow{{AuthIndex: "ia", UsedPct: 10, ResetsAt: resetFable, ObservedAt: now}}},
	}
	if !reflect.DeepEqual(pool, want) {
		t.Fatalf("pool\n got %+v\nwant %+v", pool, want)
	}
}

func TestPoolWithoutDataIsEmpty(t *testing.T) {
	pool := newTestStore(observedNow, Credential{ID: "e", Index: "ie"}).Snapshot().Pool()
	if pool.Accounts != 0 || pool.WeeklyAll.Pooled != 0 || pool.Fable.Pooled != 0 {
		t.Fatalf("pool %+v, want empty", pool)
	}
}

func TestUsageFromHeadersReadsOneResponse(t *testing.T) {
	h := withWindow(http.Header{}, "7d", "allowed", "0.27", weeklyReset)
	withWindow(h, "7d_oi", "allowed", "0.10", fableReset)
	usage := UsageFromHeaders(h, observedNow)
	if usage == nil || usage.WeeklyAll == nil || usage.WeeklyAll.Percent != 27 || !usage.WeeklyAll.ResetsAt.Equal(weeklyReset) || usage.WeeklyAll.ObservedAt != observedNow {
		t.Fatalf("weekly %+v", usage)
	}
	if fable, ok := usage.Scoped[ScopeFable]; !ok || fable.Percent != 10 || !fable.ResetsAt.Equal(fableReset) {
		t.Fatalf("fable %+v", usage.Scoped)
	}
	if UsageFromHeaders(http.Header{"Content-Type": {"application/json"}}, observedNow) != nil {
		t.Fatal("headers without windows: want nil")
	}
}
