package claudequota

import (
	"net/http"
	"testing"
	"time"
)

// window builds the three headers of one window; empty values are omitted.
func window(h http.Header, name, status, utilization string, reset time.Time) http.Header {
	prefix := "Anthropic-Ratelimit-Unified-" + name + "-"
	if status != "" {
		h.Set(prefix+"Status", status)
	}
	if utilization != "" {
		h.Set(prefix+"Utilization", utilization)
	}
	if !reset.IsZero() {
		h.Set(prefix+"Reset", unix(reset)[0])
	}
	return h
}

func newTestStore(now time.Time, creds ...Credential) *Store {
	byID := map[string]Credential{}
	for _, cred := range creds {
		byID[cred.ID] = cred
	}
	return NewStore(
		func() []Credential { return creds },
		func(id string) (Credential, bool) { cred, ok := byID[id]; return cred, ok },
		func() time.Time { return now },
	)
}

// pair: healthy has headroom everywhere; exhausted has weekly_all rejected but Fable at 40.
func pair(t *testing.T) (healthy, exhausted Credential) {
	h := window(http.Header{}, "5h", "allowed", "0.31", sessionReset)
	window(h, "7d", "allowed", "0.09", weeklyReset)
	window(h, "7d_oi", "allowed", "0.07", fableReset)
	e := window(http.Header{}, "5h", "allowed", "0.00", time.Time{})
	window(e, "7d", "rejected", "1.00", weeklyReset)
	window(e, "7d_oi", "allowed", "0.40", fableReset)
	return Credential{ID: "healthy", Name: "a", Observations: []Observation{observe(t, h, observedNow)}},
		Credential{ID: "exhausted", Name: "b", Observations: []Observation{observe(t, e, observedNow)}}
}

func accountByID(t *testing.T, snap Snapshot, id string) Account {
	t.Helper()
	for _, account := range snap.Accounts {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %q missing from %+v", id, snap.Accounts)
	return Account{}
}

func TestSnapshotReportsQuotaPerAccountAndSummary(t *testing.T) {
	healthy, exhausted := pair(t)
	pending := Credential{ID: "pending", Name: "c"}
	store := newTestStore(observedNow, healthy, exhausted, pending)

	snap := store.Snapshot()
	if !snap.Enabled {
		t.Fatal("snapshot enabled = false, want always true")
	}
	a := accountByID(t, snap, "healthy")
	if a.State != StateOK || a.Stale || a.ObservedAt == nil || !a.ObservedAt.Equal(observedNow) || !a.HasQuota.Any || !a.HasQuota.Scoped[ScopeFable] {
		t.Fatalf("healthy = %+v, want ok, fresh, observed_at and Fable quota", a)
	}
	b := accountByID(t, snap, "exhausted")
	if b.HasQuota.Any || b.HasQuota.Scoped[ScopeFable] || b.WeeklyScoped[ScopeFable].Percent != 40 || b.WeeklyAll.Percent != 100 {
		t.Fatalf("exhausted = %+v, want no quota at weekly_all 100 despite Fable 40", b)
	}
	if c := accountByID(t, snap, "pending"); c.State != StatePending || c.HasQuota != nil || c.ObservedAt != nil {
		t.Fatalf("pending = %+v, want pending without data", c)
	}

	sum := snap.Summary
	if sum.Accounts.Total != 3 || sum.Accounts.WithData != 2 || sum.Accounts.Pending != 1 || sum.Accounts.WithQuotaAny != 1 {
		t.Fatalf("summary accounts = %+v", sum.Accounts)
	}
	// Means of (9, 100) and (31, 0); Fable used mean(7, 40), remaining mean(91, 0).
	fable := sum.Scoped[ScopeFable]
	if *sum.WeeklyAllUsedPct != 54.5 || *sum.SessionUsedPct != 15.5 || fable.UsedPct != 23.5 || fable.RemainingPct != 45.5 || fable.AccountsWithQuota != 1 || fable.Accounts != 2 {
		t.Fatalf("summary = %+v, Fable %+v", sum, fable)
	}
	if sum.SoonestReset == nil || !sum.SoonestReset.Equal(sessionReset) {
		t.Fatalf("soonest_reset = %v, want %v", sum.SoonestReset, sessionReset)
	}
}

func TestStatusFromObservedWindows(t *testing.T) {
	healthy, exhausted := pair(t)
	store := newTestStore(observedNow, healthy, exhausted, Credential{ID: "pending"})
	for _, tc := range []struct {
		id, model string
		want      Status
	}{
		{"healthy", "claude-fable-5", Status{Known: true, OK: true}},
		{"exhausted", "claude-fable-5", Status{Known: true, ResetAt: weeklyReset}},
		{"exhausted", "claude-sonnet-4-5", Status{Known: true, ResetAt: weeklyReset}},
		{"pending", "claude-fable-5", Status{OK: true}},
		{"missing", "claude-fable-5", Status{OK: true}},
	} {
		if got := store.Status(tc.id, tc.model); got != tc.want {
			t.Errorf("Status(%s, %s) = %+v, want %+v", tc.id, tc.model, got, tc.want)
		}
	}
	afterReset := newTestStore(weeklyReset, healthy, exhausted)
	if got := afterReset.Status("exhausted", "claude-sonnet-4-5"); got != (Status{Known: true, OK: true}) {
		t.Fatalf("Status past the weekly reset = %+v, want known and available", got)
	}
	if !accountByID(t, afterReset.Snapshot(), "exhausted").Stale {
		t.Fatal("account past its weekly reset not marked stale")
	}
}

func TestDisabledCredentialIsListedButNotSummarised(t *testing.T) {
	healthy, exhausted := pair(t)
	exhausted.Disabled = true
	snap := newTestStore(observedNow, healthy, exhausted).Snapshot()
	if b := accountByID(t, snap, "exhausted"); !b.Disabled || b.State != StateDisabled {
		t.Fatalf("disabled account = %+v, want disabled flag and state", b)
	}
	if snap.Summary.Accounts.Total != 1 || snap.Summary.Accounts.Disabled != 1 || *snap.Summary.WeeklyAllUsedPct != 9 {
		t.Fatalf("summary = %+v, want only the enabled account counted", snap.Summary)
	}
}

func TestQuotaCooldownBlocksAndExtendsTheReset(t *testing.T) {
	healthy, exhausted := pair(t)
	cooldown := observedNow.Add(time.Hour)
	healthy.CooldownUntil = cooldown
	later := weeklyReset.Add(time.Hour)
	exhausted.ModelCooldowns = map[string]time.Time{"claude-fable-5": later}
	store := newTestStore(observedNow, healthy, exhausted)

	if got := store.Status("healthy", "claude-fable-5"); got != (Status{Known: true, ResetAt: cooldown}) {
		t.Fatalf("Status(cooled healthy) = %+v, want blocked until %v", got, cooldown)
	}
	if got := store.Status("exhausted", "claude-fable-5"); got != (Status{Known: true, ResetAt: later}) {
		t.Fatalf("Status(exhausted cooled past its weekly reset) = %+v, want blocked until %v", got, later)
	}
	a := accountByID(t, store.Snapshot(), "healthy")
	if a.HasQuota.Any || a.CooldownUntil == nil || !a.CooldownUntil.Equal(cooldown) {
		t.Fatalf("cooled account = %+v, want no quota and cooldown_until %v", a, cooldown)
	}
}
