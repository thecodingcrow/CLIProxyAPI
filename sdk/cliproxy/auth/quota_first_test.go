package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// fakeQuota answers from a mutable in-memory table; absent IDs are untracked.
type fakeQuota struct {
	mu     sync.Mutex
	status map[string]QuotaStatus
}

func (f *fakeQuota) QuotaStatus(authID, _ string) QuotaStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[authID]
}

func (f *fakeQuota) set(authID string, status QuotaStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		f.status = map[string]QuotaStatus{}
	}
	f.status[authID] = status
}

var (
	quotaOK        = QuotaStatus{Known: true, Available: true}
	quotaUnknown   = QuotaStatus{Tracked: true}
	quotaResetNext = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	quotaResetLate = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
)

func quotaExhausted(resetAt time.Time) QuotaStatus { return QuotaStatus{Known: true, ResetAt: resetAt} }

const quotaFirstModel = "claude-fable-5"

type quotaFirstHarness struct {
	t        *testing.T
	manager  *Manager
	provider string
	mixed    bool
}

// newQuotaFirstHarness registers ids for one provider serving quotaFirstModel.
func newQuotaFirstHarness(t *testing.T, provider string, selector Selector, quota QuotaProvider, ids ...string) *quotaFirstHarness {
	t.Helper()
	manager := NewManager(nil, selector, nil)
	if quota != nil {
		manager.SetQuotaProvider(quota)
	}
	manager.RegisterExecutor(schedulerTestExecutor{provider: provider})
	for _, id := range ids {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{ID: id, Provider: provider, Status: StatusActive}); errRegister != nil {
			t.Fatalf("Register(%s): %v", id, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: quotaFirstModel}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return &quotaFirstHarness{t: t, manager: manager, provider: provider}
}

func (h *quotaFirstHarness) pick(opts cliproxyexecutor.Options, tried map[string]struct{}) string {
	h.t.Helper()
	var picked *Auth
	var errPick error
	if h.mixed {
		picked, _, _, errPick = h.manager.pickNextMixed(context.Background(), []string{h.provider}, quotaFirstModel, opts, tried)
	} else {
		picked, _, errPick = h.manager.pickNext(context.Background(), h.provider, quotaFirstModel, opts, tried)
	}
	if errPick != nil {
		h.t.Fatalf("pick: %v", errPick)
	}
	return picked.ID
}

func (h *quotaFirstHarness) picks(n int) []string {
	h.t.Helper()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = h.pick(cliproxyexecutor.Options{}, nil)
	}
	return ids
}

func (h *quotaFirstHarness) quotaExceeded(id string) {
	h.manager.MarkResult(context.Background(), Result{
		AuthID: id, Provider: h.provider, Model: quotaFirstModel, Success: false,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})
}

func assertSequence(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

func TestQuotaFirstWithoutQuotaDataMatchesRoundRobin(t *testing.T) {
	// sequence returns credential suffixes: four picks, a retry excluding the
	// next in line, three more picks. The registry is global, so each run
	// registers its own IDs.
	sequence := func(t *testing.T, provider string, selector Selector, quota QuotaProvider) []string {
		h := newQuotaFirstHarness(t, provider, selector, quota, provider+"-a", provider+"-b", provider+"-c")
		got := h.picks(4)
		got = append(got, h.pick(cliproxyexecutor.Options{}, map[string]struct{}{provider + "-b": {}}))
		got = append(got, h.picks(3)...)
		for i := range got {
			got[i] = got[i][len(provider)+1:]
		}
		return got
	}
	want := []string{"a", "b", "c", "a", "c", "a", "b", "c"}

	assertSequence(t, "round-robin", sequence(t, "qf-rr-baseline", nil, nil), want...)
	for name, quota := range map[string]QuotaProvider{
		"none":      nil,
		"off":       &fakeQuota{},
		"unknown":   &fakeQuota{status: map[string]QuotaStatus{"qf-rr-unknown-a": quotaUnknown, "qf-rr-unknown-b": quotaUnknown, "qf-rr-unknown-c": quotaUnknown}},
		"untracked": &fakeQuota{status: map[string]QuotaStatus{"qf-rr-untracked-a": {}}},
	} {
		t.Run(name, func(t *testing.T) {
			assertSequence(t, "quota-first", sequence(t, "qf-rr-"+name, &QuotaFirstSelector{}, quota), want...)
		})
	}
}

func TestQuotaFirstRoundRobinsAcrossHealthyCredentials(t *testing.T) {
	quota := &fakeQuota{}
	quota.set("qf-healthy-a", quotaOK)
	quota.set("qf-healthy-b", quotaOK)
	h := newQuotaFirstHarness(t, "qf-healthy", &QuotaFirstSelector{}, quota, "qf-healthy-a", "qf-healthy-b")
	assertSequence(t, "picks", h.picks(4), "qf-healthy-a", "qf-healthy-b", "qf-healthy-a", "qf-healthy-b")
}

func TestQuotaFirstAvoidsCredentialExhaustedForTheModel(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := map[bool]string{false: "single provider", true: "mixed provider"}[mixed]
		t.Run(name, func(t *testing.T) {
			provider := map[bool]string{false: "qf-avoid-single", true: "qf-avoid-mixed"}[mixed]
			quota := &fakeQuota{}
			quota.set(provider+"-exhausted", quotaExhausted(quotaResetNext))
			quota.set(provider+"-healthy", quotaOK)
			h := newQuotaFirstHarness(t, provider, &QuotaFirstSelector{}, quota, provider+"-exhausted", provider+"-healthy")
			h.mixed = mixed

			healthy := provider + "-healthy"
			assertSequence(t, "picks", h.picks(3), healthy, healthy, healthy)
			// Exhausted is demoted, never blocked: a retry past the healthy one still reaches it.
			if got := h.pick(cliproxyexecutor.Options{}, map[string]struct{}{healthy: {}}); got != provider+"-exhausted" {
				t.Fatalf("retry excluding the healthy credential = %q, want the exhausted one", got)
			}
		})
	}
}

func TestQuotaFirstPrefersTheCredentialWhoseBlockEndsSoonest(t *testing.T) {
	quota := &fakeQuota{}
	quota.set("qf-soon-a-late", quotaExhausted(quotaResetLate))
	quota.set("qf-soon-b-next", quotaExhausted(quotaResetNext))
	quota.set("qf-soon-c-unreported", quotaExhausted(time.Time{}))
	quota.set("qf-soon-d-next", quotaExhausted(quotaResetNext))
	h := newQuotaFirstHarness(t, "qf-soon", &QuotaFirstSelector{}, quota, "qf-soon-a-late", "qf-soon-b-next", "qf-soon-c-unreported", "qf-soon-d-next")

	assertSequence(t, "picks among equal soonest resets", h.picks(3), "qf-soon-b-next", "qf-soon-d-next", "qf-soon-b-next")

	quota.set("qf-soon-b-next", quotaExhausted(quotaResetLate))
	quota.set("qf-soon-d-next", quotaExhausted(quotaResetLate))
	assertSequence(t, "picks when reported resets tie, unreported last", h.picks(2), "qf-soon-d-next", "qf-soon-a-late")
}

func TestQuotaFirstRanksUnknownWithHealthyAndAboveExhausted(t *testing.T) {
	quota := &fakeQuota{}
	quota.set("qf-mix-a-healthy", quotaOK)
	quota.set("qf-mix-b-unknown", quotaUnknown)
	quota.set("qf-mix-c-exhausted", quotaExhausted(quotaResetNext))
	h := newQuotaFirstHarness(t, "qf-mix", &QuotaFirstSelector{}, quota, "qf-mix-a-healthy", "qf-mix-b-unknown", "qf-mix-c-exhausted")

	assertSequence(t, "picks", h.picks(4), "qf-mix-a-healthy", "qf-mix-b-unknown", "qf-mix-a-healthy", "qf-mix-b-unknown")

	quota.set("qf-mix-a-healthy", quotaExhausted(quotaResetNext))
	assertSequence(t, "picks once the healthy one is exhausted", h.picks(2), "qf-mix-b-unknown", "qf-mix-b-unknown")
}

// quotaLeft is a known-good status; resets are offsets from one now so picks
// compare them against the same wall clock within hours of margin.
func quotaLeft(headroom float64, drainBy time.Time) QuotaStatus {
	return QuotaStatus{Known: true, Available: true, HeadroomPct: headroom, DrainBy: drainBy}
}

func TestQuotaFirstDrainsTheCredentialWithMoreHeadroomFirst(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour)
	quota := &fakeQuota{}
	quota.set("qf-drain-a", quotaLeft(30, reset))
	quota.set("qf-drain-b", quotaLeft(80, reset))
	h := newQuotaFirstHarness(t, "qf-drain", &QuotaFirstSelector{}, quota, "qf-drain-a", "qf-drain-b")

	assertSequence(t, "picks at 30 vs 80", h.picks(3), "qf-drain-b", "qf-drain-b", "qf-drain-b")
	quota.set("qf-drain-b", quotaLeft(29, reset))
	assertSequence(t, "picks once b falls below a", h.picks(2), "qf-drain-a", "qf-drain-a")
	quota.set("qf-drain-b", quotaLeft(30, reset))
	assertSequence(t, "picks at equal headroom", h.picks(2), "qf-drain-b", "qf-drain-a")
}

func TestQuotaFirstDrainsTheSoonerResetFirst(t *testing.T) {
	monday := time.Now() // read as Monday 12:00
	tuesday, thursday := monday.Add(24*time.Hour), monday.Add(74*time.Hour)
	quota := &fakeQuota{}
	quota.set("qf-reset-a-thu", quotaLeft(50, thursday))
	quota.set("qf-reset-b-tue", quotaLeft(50, tuesday))
	h := newQuotaFirstHarness(t, "qf-reset", &QuotaFirstSelector{}, quota, "qf-reset-a-thu", "qf-reset-b-tue")

	assertSequence(t, "picks at equal headroom", h.picks(2), "qf-reset-b-tue", "qf-reset-b-tue")
	// 5/24h is less pressure than 90/74h: a nearly drained account does not
	// outrank one with far more to spend before its own reset.
	quota.set("qf-reset-a-thu", quotaLeft(90, thursday))
	quota.set("qf-reset-b-tue", quotaLeft(5, tuesday))
	assertSequence(t, "picks at Tue 5 vs Thu 90", h.picks(2), "qf-reset-a-thu", "qf-reset-a-thu")
}

func TestQuotaFirstTreatsAnUnreportedResetAsAWeekAway(t *testing.T) {
	now := time.Now()
	quota := &fakeQuota{}
	quota.set("qf-horizon-a-unreported", quotaLeft(50, time.Time{}))
	quota.set("qf-horizon-b", quotaLeft(50, now.Add(24*time.Hour)))
	h := newQuotaFirstHarness(t, "qf-horizon", &QuotaFirstSelector{}, quota, "qf-horizon-a-unreported", "qf-horizon-b")

	assertSequence(t, "picks against a reset in a day", h.picks(2), "qf-horizon-b", "qf-horizon-b")
	quota.set("qf-horizon-b", quotaLeft(50, now.Add(200*time.Hour)))
	assertSequence(t, "picks against a reset past a week", h.picks(2), "qf-horizon-a-unreported", "qf-horizon-a-unreported")
}

func TestQuotaFirstSharesTheTopTierWithAnUnknownCredential(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour)
	quota := &fakeQuota{}
	quota.set("qf-probe-a", quotaLeft(80, reset))
	quota.set("qf-probe-b", quotaLeft(30, reset))
	quota.set("qf-probe-c-unknown", quotaUnknown)
	h := newQuotaFirstHarness(t, "qf-probe", &QuotaFirstSelector{}, quota, "qf-probe-a", "qf-probe-b", "qf-probe-c-unknown")

	assertSequence(t, "picks", h.picks(4), "qf-probe-a", "qf-probe-c-unknown", "qf-probe-a", "qf-probe-c-unknown")
}

func TestQuotaFirstServesUntrackedOnlyWithoutPlanQuotaAndBeforeExhausted(t *testing.T) {
	quota := &fakeQuota{}
	quota.set("qf-tier-a-leader", quotaLeft(80, time.Now().Add(72*time.Hour)))
	quota.set("qf-tier-b-unknown", quotaUnknown)
	h := newQuotaFirstHarness(t, "qf-tier", &QuotaFirstSelector{}, quota, "qf-tier-a-leader", "qf-tier-b-unknown", "qf-tier-c-untracked")

	assertSequence(t, "picks", h.picks(4), "qf-tier-a-leader", "qf-tier-b-unknown", "qf-tier-a-leader", "qf-tier-b-unknown")
	quota.set("qf-tier-a-leader", quotaExhausted(quotaResetNext))
	assertSequence(t, "picks with only the unknown left", h.picks(2), "qf-tier-b-unknown", "qf-tier-b-unknown")
	quota.set("qf-tier-b-unknown", quotaExhausted(quotaResetNext))
	assertSequence(t, "picks with every tracked one exhausted", h.picks(2), "qf-tier-c-untracked", "qf-tier-c-untracked")
}

func TestQuotaFirstClampsTimeToResetAtOneHour(t *testing.T) {
	now := time.Now()
	quota := &fakeQuota{}
	// Unclamped, a (10/0.5h = 20) would beat b (20/1.5h = 13.3).
	quota.set("qf-clamp-a", quotaLeft(10, now.Add(30*time.Minute)))
	quota.set("qf-clamp-b", quotaLeft(20, now.Add(90*time.Minute)))
	h := newQuotaFirstHarness(t, "qf-clamp", &QuotaFirstSelector{}, quota, "qf-clamp-a", "qf-clamp-b")
	assertSequence(t, "picks inside the last hours", h.picks(2), "qf-clamp-b", "qf-clamp-b")

	// A reset already past ranks by raw headroom instead of going negative.
	quota.set("qf-clamp-a", quotaLeft(50, now.Add(-time.Hour)))
	quota.set("qf-clamp-b", quotaLeft(40, now.Add(30*time.Minute)))
	assertSequence(t, "picks with a reset already past", h.picks(2), "qf-clamp-a", "qf-clamp-a")
}

func TestQuotaFirstUnderSessionAffinityKeepsBindingAndRebindsByQuota(t *testing.T) {
	quota := &fakeQuota{}
	for _, id := range []string{"qf-aff-a", "qf-aff-b", "qf-aff-c"} {
		quota.set(id, quotaOK)
	}
	affinity := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &QuotaFirstSelector{}, TTL: time.Hour})
	defer affinity.Stop()
	h := newQuotaFirstHarness(t, "qf-aff", affinity, quota, "qf-aff-a", "qf-aff-b", "qf-aff-c")
	session := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "qf-session"}}

	if got := h.pick(session, nil); got != "qf-aff-a" {
		t.Fatalf("cold binding = %q, want qf-aff-a", got)
	}
	quota.set("qf-aff-a", quotaExhausted(quotaResetNext))
	if got := h.pick(session, nil); got != "qf-aff-a" {
		t.Fatalf("bound credential once its quota data says exhausted = %q, want binding kept", got)
	}

	quota.set("qf-aff-b", quotaLeft(20, quotaResetLate))
	quota.set("qf-aff-c", quotaLeft(70, quotaResetLate))
	h.quotaExceeded("qf-aff-a")
	if got := h.pick(session, nil); got != "qf-aff-c" {
		t.Fatalf("rebinding after the bound credential cooled down = %q, want the one with most headroom", got)
	}
	if got := h.pick(session, nil); got != "qf-aff-c" {
		t.Fatalf("pick after rebinding = %q, want the new binding kept", got)
	}

	quota.set("qf-aff-b", quotaOK)
	h.quotaExceeded("qf-aff-b")
	if got := h.pick(cliproxyexecutor.Options{}, nil); got != "qf-aff-c" {
		t.Fatalf("pick without session while b cools down = %q, want cooled credential skipped", got)
	}
}
