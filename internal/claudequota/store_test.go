package claudequota

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// gate holds one credential's fetch: started closes when the fetch enters,
// open closes when the test releases it.
type gate struct {
	started chan struct{}
	open    chan struct{}
}

// fakeUpstream answers per credential ID; a held credential blocks until released.
type fakeUpstream struct {
	mu     sync.Mutex
	bodies map[string][]byte
	errs   map[string]error
	gates  map[string]*gate
	held   map[string]*gate
	calls  map[string]int
	order  []string
	tokens map[string][]string
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{bodies: map[string][]byte{}, errs: map[string]error{}, gates: map[string]*gate{}, held: map[string]*gate{}, calls: map[string]int{}, tokens: map[string][]string{}}
}

func (f *fakeUpstream) Fetch(ctx context.Context, cred Credential) (*Usage, error) {
	f.mu.Lock()
	f.calls[cred.ID]++
	f.order = append(f.order, cred.ID)
	f.tokens[cred.ID] = append(f.tokens[cred.ID], cred.AccessToken)
	// A gate holds only the next fetch; later fetches pass straight through.
	g := f.gates[cred.ID]
	delete(f.gates, cred.ID)
	if g != nil {
		f.held[cred.ID] = g
	}
	f.mu.Unlock()
	if g != nil {
		close(g.started)
		select {
		case <-g.open:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[cred.ID]; err != nil {
		return nil, err
	}
	return ParseUsage(f.bodies[cred.ID])
}

func (f *fakeUpstream) set(id string, body []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[id] = body
	f.errs[id] = err
}

// hold blocks the next fetch of id; the returned channel closes once it starts.
func (f *fakeUpstream) hold(id string) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := &gate{started: make(chan struct{}), open: make(chan struct{})}
	f.gates[id] = g
	return g.started
}

func (f *fakeUpstream) release(id string) {
	f.mu.Lock()
	g := f.held[id]
	delete(f.held, id)
	f.mu.Unlock()
	if g != nil {
		close(g.open)
	}
}

func (f *fakeUpstream) callCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func (f *fakeUpstream) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.order)
}

type fakeSource struct {
	mu    sync.Mutex
	creds []Credential
}

func (s *fakeSource) list() []Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Credential(nil), s.creds...)
}

func (s *fakeSource) update(id string, mutate func(*Credential)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.creds {
		if s.creds[i].ID == id {
			mutate(&s.creds[i])
		}
	}
}

type harness struct {
	clock    *fakeClock
	upstream *fakeUpstream
	source   *fakeSource
	store    *Store

	mu       sync.Mutex
	settings Settings
	pauses   []time.Duration
	jitter   float64
	// onPause runs inside every spacing pause of a round, outside h.mu.
	onPause func()
}

func newHarness(t *testing.T, creds ...Credential) *harness {
	t.Helper()
	h := &harness{
		clock:    &fakeClock{now: fixtureNow},
		upstream: newFakeUpstream(),
		source:   &fakeSource{creds: creds},
		settings: Settings{Enabled: true},
	}
	h.store = NewStore(h.source.list, h.upstream, h.currentSettings, Options{
		Now: h.clock.Now,
		Pause: func(_ context.Context, d time.Duration) bool {
			h.mu.Lock()
			h.pauses = append(h.pauses, d)
			hook := h.onPause
			h.mu.Unlock()
			if hook != nil {
				hook()
			}
			return true
		},
		Jitter: func() float64 {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.jitter
		},
	})
	return h
}

func (h *harness) currentSettings() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}

func (h *harness) setOnPause(hook func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onPause = hook
}

func (h *harness) setInterval(interval time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settings.Interval = interval
}

func (h *harness) setEnabled(enabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settings.Enabled = enabled
}

// tick runs one tick and waits for the round it started.
func (h *harness) tick() {
	<-h.store.Tick(context.Background())
}

// newPairHarness wires the two recorded accounts: "healthy" and "exhausted".
func newPairHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t,
		Credential{ID: "healthy", Index: "idx-healthy", Email: "healthy@example.test", AccessToken: "fake-healthy"},
		Credential{ID: "exhausted", Index: "idx-exhausted", Email: "exhausted@example.test", AccessToken: "fake-exhausted"},
	)
	h.upstream.set("healthy", loadFixture(t, "usage-healthy.json"), nil)
	h.upstream.set("exhausted", loadFixture(t, "usage-weekly-exhausted.json"), nil)
	return h
}

// newPolledPairHarness has completed the first poll of both accounts.
func newPolledPairHarness(t *testing.T) *harness {
	t.Helper()
	h := newPairHarness(t)
	h.tick()
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()
	return h
}

func accountByID(t *testing.T, snap Snapshot, id string) Account {
	t.Helper()
	for _, account := range snap.Accounts {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %q missing from snapshot", id)
	return Account{}
}

func TestDisabledPollingNeverFetchesAndReportsDisabled(t *testing.T) {
	h := newPairHarness(t)
	h.setEnabled(false)
	for i := 0; i < 5; i++ {
		h.tick()
		h.clock.Advance(MaxBackoff)
	}

	snap := h.store.Snapshot()
	if snap.Enabled || len(snap.Accounts) != 0 || snap.RefreshIntervalSeconds != 0 {
		t.Fatalf("snapshot = %+v, want enabled=false with no accounts", snap)
	}
	if got := h.upstream.totalCalls(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0 while disabled", got)
	}
	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); !ok || known {
		t.Fatalf("HasQuota while disabled = %v/%v, want true/false (unknown never blocks)", ok, known)
	}
}

func TestFirstPollWaitsOneFullInterval(t *testing.T) {
	h := newPairHarness(t)
	h.tick()
	h.clock.Advance(DefaultRefreshInterval - time.Second)
	h.tick()

	if got := h.upstream.totalCalls(); got != 0 {
		t.Fatalf("upstream calls before one interval = %d, want 0", got)
	}
	snap := h.store.Snapshot()
	for _, account := range snap.Accounts {
		if account.State != StatePending || account.FetchedAt != nil || account.HasQuota != nil {
			t.Fatalf("account %s = %+v, want pending without data", account.ID, account)
		}
	}
	if snap.Summary.Accounts.Total != 2 || snap.Summary.Accounts.Pending != 2 || snap.Summary.WeeklyAllUsedPct != nil {
		t.Fatalf("summary = %+v, want 2 pending accounts and no percentages", snap.Summary)
	}
	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); !ok || known {
		t.Fatalf("HasQuota before first poll = %v/%v, want true/false", ok, known)
	}

	h.clock.Advance(time.Second)
	h.tick()
	if h.upstream.callCount("healthy") != 1 || h.upstream.callCount("exhausted") != 1 {
		t.Fatalf("calls after one interval = %d/%d, want 1/1", h.upstream.callCount("healthy"), h.upstream.callCount("exhausted"))
	}
}

func TestRoundFetchesSeriallyWithSpacing(t *testing.T) {
	h := newPolledPairHarness(t)
	if len(h.pauses) != 1 || h.pauses[0] != fetchSpacing {
		t.Fatalf("pauses = %v, want one %v gap between two fetches", h.pauses, fetchSpacing)
	}
}

func TestPolledPairReportsQuotaPerAccountAndSummary(t *testing.T) {
	h := newPolledPairHarness(t)

	snap := h.store.Snapshot()
	if !snap.Enabled || snap.RefreshIntervalSeconds != 600 {
		t.Fatalf("snapshot enabled/interval = %v/%d, want true/600", snap.Enabled, snap.RefreshIntervalSeconds)
	}
	healthy := accountByID(t, snap, "healthy")
	if healthy.State != StateOK || healthy.Stale || healthy.HasQuota == nil || !healthy.HasQuota.Any || !healthy.HasQuota.Scoped["Fable"] {
		t.Fatalf("healthy = %+v, want ok with Fable quota", healthy)
	}
	exhausted := accountByID(t, snap, "exhausted")
	if exhausted.HasQuota == nil || exhausted.HasQuota.Any || exhausted.HasQuota.Scoped["Fable"] {
		t.Fatalf("exhausted has_quota = %+v, want no quota for any model or Fable", exhausted.HasQuota)
	}
	if exhausted.WeeklyScoped["Fable"].Percent != 40 || exhausted.WeeklyAll.Percent != 100 {
		t.Fatalf("exhausted limits = %+v / %+v, want weekly_all 100 and Fable 40", exhausted.WeeklyAll, exhausted.WeeklyScoped)
	}

	sum := snap.Summary
	if sum.Accounts.Total != 2 || sum.Accounts.WithData != 2 || sum.Accounts.WithQuotaAny != 1 || sum.Averaging != AveragingMean {
		t.Fatalf("summary = %+v, want 2 total, 2 with data, 1 with quota, mean averaging", sum)
	}
	// Means of (9, 100) and (31, 0).
	if sum.WeeklyAllUsedPct == nil || *sum.WeeklyAllUsedPct != 54.5 {
		t.Fatalf("weekly_all_used_pct = %v, want 54.5", sum.WeeklyAllUsedPct)
	}
	if sum.SessionUsedPct == nil || *sum.SessionUsedPct != 15.5 {
		t.Fatalf("session_used_pct = %v, want 15.5", sum.SessionUsedPct)
	}
	// used = mean(7, 40); remaining = mean(100-max(7,9), 100-max(40,100)) = mean(91, 0).
	fable := sum.Scoped["Fable"]
	if fable.UsedPct != 23.5 || fable.RemainingPct != 45.5 || fable.AccountsWithQuota != 1 || fable.Accounts != 2 {
		t.Fatalf("scoped Fable = %+v, want used 23.5, remaining 45.5, 1 of 2 accounts with quota", fable)
	}
	wantReset := time.Date(2026, 10, 15, 11, 59, 59, 687114000, time.UTC)
	if sum.SoonestReset == nil || !sum.SoonestReset.Equal(wantReset) {
		t.Fatalf("soonest_reset = %v, want %v", sum.SoonestReset, wantReset)
	}

	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); !ok || !known {
		t.Fatalf("HasQuota(healthy, fable) = %v/%v, want true/true", ok, known)
	}
	if ok, known := h.store.HasQuota("exhausted", "claude-fable-5"); ok || !known {
		t.Fatalf("HasQuota(exhausted, fable) = %v/%v, want false/true", ok, known)
	}
	if ok, known := h.store.HasQuota("exhausted", "claude-sonnet-4-5"); ok || !known {
		t.Fatalf("HasQuota(exhausted, sonnet) = %v/%v, want false/true", ok, known)
	}
	if ok, known := h.store.HasQuota("missing", "claude-fable-5"); !ok || known {
		t.Fatalf("HasQuota(missing) = %v/%v, want true/false", ok, known)
	}
}

func TestSummaryListsUnknownScopedModels(t *testing.T) {
	h := newHarness(t, Credential{ID: "a", AccessToken: "fake-a"})
	h.upstream.set("a", []byte(`{"limits":[
		{"kind":"weekly_all","percent":20,"scope":null},
		{"kind":"weekly_scoped","percent":30,"resets_at":null,"scope":{"model":{"display_name":"Nimbus Max"}}}
	]}`), nil)
	h.tick()
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()

	scoped, ok := h.store.Snapshot().Summary.Scoped["Nimbus Max"]
	if !ok || scoped.UsedPct != 30 || scoped.RemainingPct != 70 || scoped.AccountsWithQuota != 1 {
		t.Fatalf("summary.scoped[Nimbus Max] = %+v (present %v), want used 30, remaining 70, 1 account", scoped, ok)
	}
}

func TestRunningRoundIsNotDuplicatedAndServesPreviousValue(t *testing.T) {
	h := newPolledPairHarness(t)
	h.upstream.set("healthy", loadFixture(t, "usage-weekly-exhausted.json"), nil)
	started := h.upstream.hold("healthy")
	h.clock.Advance(DefaultRefreshInterval)
	done := h.store.Tick(context.Background())
	<-started

	h.clock.Advance(DefaultRefreshInterval)
	h.tick()
	if got := h.upstream.callCount("healthy"); got != 2 {
		t.Fatalf("healthy fetches = %d, want 2 (first poll + one in flight, no duplicate)", got)
	}
	during := accountByID(t, h.store.Snapshot(), "healthy")
	if !during.Refreshing || during.WeeklyAll.Percent != 9 {
		t.Fatalf("healthy during refresh = %+v, want refreshing with previous weekly_all 9", during)
	}

	h.upstream.release("healthy")
	<-done
	after := accountByID(t, h.store.Snapshot(), "healthy")
	if after.Refreshing || after.Stale || after.WeeklyAll.Percent != 100 {
		t.Fatalf("healthy after refresh = %+v, want fresh weekly_all 100", after)
	}
}

func TestOverdueValueIsMarkedStale(t *testing.T) {
	h := newPolledPairHarness(t)
	started := h.upstream.hold("healthy")
	h.clock.Advance(DefaultRefreshInterval)
	done := h.store.Tick(context.Background())
	<-started
	h.clock.Advance(DefaultRefreshInterval + time.Second)

	stale := accountByID(t, h.store.Snapshot(), "healthy")
	if !stale.Stale || stale.WeeklyAll == nil || stale.WeeklyAll.Percent != 9 {
		t.Fatalf("healthy with overdue refresh = %+v, want stale previous value", stale)
	}
	h.upstream.release("healthy")
	<-done
}

func TestUpstreamErrorKeepsLastGoodValueAndBacksOff(t *testing.T) {
	h := newPolledPairHarness(t)
	lastGood := h.clock.Now()
	h.upstream.set("healthy", nil, errors.New("fetch Claude OAuth usage failed with status 429"))
	h.upstream.set("exhausted", loadFixture(t, "usage-healthy.json"), nil)
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()

	snap := h.store.Snapshot()
	healthy := accountByID(t, snap, "healthy")
	if healthy.State != StateError || !healthy.Stale || healthy.Error == "" || healthy.WeeklyAll.Percent != 9 || !healthy.FetchedAt.Equal(lastGood) {
		t.Fatalf("healthy after error = %+v, want error, stale, last good weekly_all 9 fetched %v", healthy, lastGood)
	}
	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); !ok || !known {
		t.Fatalf("HasQuota(healthy) after error = %v/%v, want last good value true/true", ok, known)
	}
	exhausted := accountByID(t, snap, "exhausted")
	if exhausted.State != StateOK || exhausted.WeeklyAll.Percent != 9 {
		t.Fatalf("exhausted = %+v, want refreshed despite the other account failing", exhausted)
	}

	// One failure doubles the interval: no retry at +10m, retry at +20m.
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()
	if got := h.upstream.callCount("healthy"); got != 2 {
		t.Fatalf("healthy fetches one interval after the error = %d, want 2 (backing off)", got)
	}
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()
	if got := h.upstream.callCount("healthy"); got != 3 {
		t.Fatalf("healthy fetches two intervals after the error = %d, want 3", got)
	}
}

func TestBackoffAddsJitterAndCapsAtMaxBackoff(t *testing.T) {
	h := newHarness(t, Credential{ID: "a", AccessToken: "fake-a"})
	h.upstream.set("a", nil, errors.New("fetch Claude OAuth usage failed with status 500"))
	h.jitter = 0.5
	h.tick()
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()

	// 10m doubled once, plus 0.5 * 20% jitter.
	next := accountByID(t, h.store.Snapshot(), "a").NextPollAt
	if want := h.clock.Now().Add(22 * time.Minute); next == nil || !next.Equal(want) {
		t.Fatalf("next poll after one failure = %v, want %v", next, want)
	}
	for i := 0; i < 6; i++ {
		h.clock.Advance(MaxBackoff + MaxBackoff/5)
		h.tick()
	}
	// Capped at 1h, jitter applied after the cap.
	next = accountByID(t, h.store.Snapshot(), "a").NextPollAt
	if want := h.clock.Now().Add(66 * time.Minute); next == nil || !next.Equal(want) {
		t.Fatalf("next poll after many failures = %v, want capped %v", next, want)
	}
}

func TestBackoffNeverRetriesSoonerThanALongInterval(t *testing.T) {
	h := newHarness(t, Credential{ID: "a", AccessToken: "fake-a"})
	h.setInterval(2 * time.Hour)
	h.upstream.set("a", nil, errors.New("fetch Claude OAuth usage failed with status 503"))
	h.jitter = 0.5
	h.tick()
	h.clock.Advance(2 * time.Hour)
	h.tick()

	// Ceiling is the 2h interval, not the 1h backoff cap; plus 0.5 * 20% jitter.
	next := accountByID(t, h.store.Snapshot(), "a").NextPollAt
	if want := h.clock.Now().Add(132 * time.Minute); next == nil || !next.Equal(want) {
		t.Fatalf("next poll after failure with a 2h interval = %v, want %v", next, want)
	}
}

// roundWithPauseHook seeds the pair, then runs the first round with hook
// between its two fetches ("exhausted" first, then "healthy").
func roundWithPauseHook(t *testing.T, hook func(h *harness)) *harness {
	t.Helper()
	h := newPairHarness(t)
	h.tick()
	h.clock.Advance(DefaultRefreshInterval)
	h.setOnPause(func() { hook(h) })
	h.tick()
	h.setOnPause(nil)
	return h
}

func TestRoundStopsWhenPollingIsDisabledMidRound(t *testing.T) {
	h := roundWithPauseHook(t, func(h *harness) { h.setEnabled(false) })
	if got := h.upstream.callCount("healthy"); got != 0 || h.upstream.totalCalls() != 1 {
		t.Fatalf("calls after disabling mid-round = %v, want only the fetch before the pause", h.upstream.order)
	}
}

func TestRoundSkipsCredentialDisabledMidRound(t *testing.T) {
	h := roundWithPauseHook(t, func(h *harness) {
		h.source.update("healthy", func(c *Credential) { c.Disabled = true })
	})
	if got := h.upstream.callCount("healthy"); got != 0 {
		t.Fatalf("healthy fetches after it was disabled mid-round = %d, want 0", got)
	}
}

func TestReenablingMidRoundWaitsAFullInterval(t *testing.T) {
	h := roundWithPauseHook(t, func(h *harness) {
		h.setEnabled(false)
		h.store.Snapshot()
		h.setEnabled(true)
	})
	if got := h.upstream.totalCalls(); got != 1 {
		t.Fatalf("calls in the round that saw disable+re-enable = %v, want only the fetch before the pause", h.upstream.order)
	}
	h.clock.Advance(DefaultRefreshInterval - time.Second)
	h.tick()
	if got := h.upstream.totalCalls(); got != 1 {
		t.Fatalf("calls before a full interval after re-enabling = %d, want 1", got)
	}
	h.clock.Advance(time.Second)
	h.tick()
	if got := h.upstream.totalCalls(); got != 3 {
		t.Fatalf("calls one interval after re-enabling = %d, want 3", got)
	}
}

func TestRoundSendsTokenRotatedMidRound(t *testing.T) {
	h := roundWithPauseHook(t, func(h *harness) {
		h.source.update("healthy", func(c *Credential) { c.AccessToken = "fake-healthy-rotated" })
	})
	h.upstream.mu.Lock()
	defer h.upstream.mu.Unlock()
	if got := h.upstream.tokens["healthy"]; len(got) != 1 || got[0] != "fake-healthy-rotated" {
		t.Fatalf("healthy tokens sent = %v, want the rotated token", got)
	}
}

func TestRejectedCredentialPausesUntilTokenChanges(t *testing.T) {
	h := newPairHarness(t)
	h.upstream.set("healthy", nil, fmt.Errorf("%w: status 401", ErrCredentialRejected))
	h.tick()
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()
	for i := 0; i < 3; i++ {
		h.clock.Advance(MaxBackoff)
		h.tick()
	}
	if got := h.upstream.callCount("healthy"); got != 1 {
		t.Fatalf("rejected credential fetches = %d, want 1 (paused)", got)
	}
	if account := accountByID(t, h.store.Snapshot(), "healthy"); account.State != StateError || account.NextPollAt != nil {
		t.Fatalf("rejected account = %+v, want error state with no next poll", account)
	}

	h.upstream.set("healthy", loadFixture(t, "usage-healthy.json"), nil)
	h.source.update("healthy", func(c *Credential) { c.AccessToken = "fake-healthy-rotated" })
	h.tick()
	if got := h.upstream.callCount("healthy"); got != 2 {
		t.Fatalf("fetches after token change = %d, want 2 (resumed)", got)
	}
}

func TestDisabledCredentialIsListedButNeitherFetchedNorSummarised(t *testing.T) {
	h := newPairHarness(t)
	h.source.update("exhausted", func(c *Credential) { c.Disabled = true })
	h.tick()
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()

	snap := h.store.Snapshot()
	if got := h.upstream.callCount("exhausted"); got != 0 {
		t.Fatalf("disabled fetches = %d, want 0", got)
	}
	disabled := accountByID(t, snap, "exhausted")
	if !disabled.Disabled || disabled.State != StateDisabled {
		t.Fatalf("disabled account = %+v, want disabled flag and state", disabled)
	}
	if snap.Summary.Accounts.Total != 1 || snap.Summary.Accounts.Disabled != 1 || *snap.Summary.WeeklyAllUsedPct != 9 {
		t.Fatalf("summary = %+v, want only the enabled account counted", snap.Summary)
	}
}

func TestQuotaCooldownBlocksHasQuota(t *testing.T) {
	h := newPolledPairHarness(t)
	until := h.clock.Now().Add(10 * time.Minute)
	h.source.update("healthy", func(c *Credential) { c.CooldownUntil = until })
	snap := h.store.Snapshot()

	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); ok || !known {
		t.Fatalf("HasQuota(cooled healthy) = %v/%v, want false/true", ok, known)
	}
	account := accountByID(t, snap, "healthy")
	if account.HasQuota.Any || account.CooldownUntil == nil || !account.CooldownUntil.Equal(until) {
		t.Fatalf("cooled account = %+v, want no quota and cooldown_until %v", account, until)
	}
}

func TestStatusReportsWhenTheLastBlockingWindowResets(t *testing.T) {
	session := fixtureNow.Add(2 * time.Hour)
	weekly := fixtureNow.Add(72 * time.Hour)
	fable := fixtureNow.Add(48 * time.Hour)
	h := newHarness(t,
		Credential{ID: "both", AccessToken: "fake-both"},
		Credential{ID: "scoped", AccessToken: "fake-scoped"},
		Credential{ID: "no-reset", AccessToken: "fake-no-reset"},
	)
	h.upstream.set("both", []byte(`{"limits":[
		{"kind":"session","percent":100,"resets_at":"`+session.Format(time.RFC3339)+`"},
		{"kind":"weekly_all","percent":100,"resets_at":"`+weekly.Format(time.RFC3339)+`"}
	]}`), nil)
	h.upstream.set("scoped", []byte(`{"limits":[
		{"kind":"weekly_all","percent":50,"resets_at":"`+weekly.Format(time.RFC3339)+`"},
		{"kind":"weekly_scoped","percent":100,"resets_at":"`+fable.Format(time.RFC3339)+`","scope":{"model":{"display_name":"Fable"}}}
	]}`), nil)
	h.upstream.set("no-reset", []byte(`{"limits":[
		{"kind":"session","percent":100,"resets_at":"`+session.Format(time.RFC3339)+`"},
		{"kind":"weekly_all","percent":100,"resets_at":null}
	]}`), nil)
	h.tick()
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()

	for _, tc := range []struct {
		id, model string
		want      Status
	}{
		{"both", "claude-fable-5", Status{Known: true, ResetAt: weekly}},
		{"scoped", "claude-fable-5", Status{Known: true, ResetAt: fable}},
		{"scoped", "claude-sonnet-4-5", Status{Known: true, OK: true}},
		{"no-reset", "claude-fable-5", Status{Known: true}},
		{"missing", "claude-fable-5", Status{OK: true}},
	} {
		if got := h.store.Status(tc.id, tc.model); got.Known != tc.want.Known || got.OK != tc.want.OK || !got.ResetAt.Equal(tc.want.ResetAt) {
			t.Errorf("Status(%s, %s) = %+v, want %+v", tc.id, tc.model, got, tc.want)
		}
	}
}

func TestStatusCountsQuotaCooldownTowardsTheReset(t *testing.T) {
	h := newPolledPairHarness(t)
	weeklyReset := time.Date(2026, 10, 20, 10, 0, 0, 368360000, time.UTC)
	later := weeklyReset.Add(time.Hour)
	h.source.update("exhausted", func(c *Credential) { c.CooldownUntil = later })
	cooldown := h.clock.Now().Add(time.Hour)
	h.source.update("healthy", func(c *Credential) { c.CooldownUntil = cooldown })
	h.store.Snapshot()

	if got := h.store.Status("exhausted", "claude-fable-5"); !got.Known || got.OK || !got.ResetAt.Equal(later) {
		t.Fatalf("Status(exhausted cooled past its weekly reset) = %+v, want blocked until %v", got, later)
	}
	if got := h.store.Status("healthy", "claude-fable-5"); !got.Known || got.OK || !got.ResetAt.Equal(cooldown) {
		t.Fatalf("Status(healthy cooled) = %+v, want blocked until %v", got, cooldown)
	}
}

func TestStatusTreatsAValueOlderThanTwoIntervalsAsUnknown(t *testing.T) {
	h := newPolledPairHarness(t)
	h.upstream.set("exhausted", nil, errors.New("fetch Claude OAuth usage failed with status 500"))
	h.clock.Advance(DefaultRefreshInterval)
	h.tick()
	if got := h.store.Status("exhausted", "claude-fable-5"); !got.Known || got.OK {
		t.Fatalf("Status one interval after a failed refresh = %+v, want last good value (blocked)", got)
	}

	h.clock.Advance(DefaultRefreshInterval + time.Second)
	if got := h.store.Status("exhausted", "claude-fable-5"); got.Known || !got.OK {
		t.Fatalf("Status beyond two intervals = %+v, want unknown and available", got)
	}
}

func TestStatusWhileDisabledDoesNotAllocate(t *testing.T) {
	h := newPairHarness(t)
	h.setEnabled(false)
	if allocs := testing.AllocsPerRun(100, func() { h.store.Status("healthy", "claude-fable-5") }); allocs != 0 {
		t.Fatalf("allocations per Status while disabled = %v, want 0", allocs)
	}
}

func TestTurningPollingOffDropsCacheAndRestartsWait(t *testing.T) {
	h := newPolledPairHarness(t)
	h.setEnabled(false)
	h.tick()
	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); !ok || known {
		t.Fatalf("HasQuota after disabling = %v/%v, want true/false", ok, known)
	}

	h.setEnabled(true)
	h.clock.Advance(MaxBackoff)
	h.tick()
	if got := h.upstream.totalCalls(); got != 2 {
		t.Fatalf("calls right after re-enabling = %d, want 2 (no immediate poll)", got)
	}
}
