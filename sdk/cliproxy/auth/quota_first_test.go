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

// fakeQuota answers from a mutable in-memory table; absent IDs are unknown.
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
		"none":    nil,
		"off":     &fakeQuota{},
		"unknown": &fakeQuota{status: map[string]QuotaStatus{"qf-rr-unknown-a": {}, "qf-rr-unknown-b": {}}},
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
	quota.set("qf-mix-c-exhausted", quotaExhausted(quotaResetNext))
	h := newQuotaFirstHarness(t, "qf-mix", &QuotaFirstSelector{}, quota, "qf-mix-a-healthy", "qf-mix-b-unknown", "qf-mix-c-exhausted")

	assertSequence(t, "picks", h.picks(4), "qf-mix-a-healthy", "qf-mix-b-unknown", "qf-mix-a-healthy", "qf-mix-b-unknown")

	quota.set("qf-mix-a-healthy", quotaExhausted(quotaResetNext))
	assertSequence(t, "picks once the healthy one is exhausted", h.picks(2), "qf-mix-b-unknown", "qf-mix-b-unknown")
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

	quota.set("qf-aff-b", quotaExhausted(quotaResetNext))
	h.quotaExceeded("qf-aff-a")
	if got := h.pick(session, nil); got != "qf-aff-c" {
		t.Fatalf("rebinding after the bound credential cooled down = %q, want the one with quota", got)
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
