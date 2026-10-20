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
			{AuthIndex: "ia", UsedPct: 27, ResetsAt: resetA},
			{AuthIndex: "ib", UsedPct: 100, ResetsAt: resetB},
		}},
		Fable: Pooled{UsedPct: 10, Pooled: 1, Windows: []AccountWindow{{AuthIndex: "ia", UsedPct: 10, ResetsAt: resetFable}}},
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
