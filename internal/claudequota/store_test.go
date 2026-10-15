package claudequota

import (
	"context"
	"errors"
	"runtime"
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
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{bodies: map[string][]byte{}, errs: map[string]error{}, gates: map[string]*gate{}, held: map[string]*gate{}, calls: map[string]int{}}
}

func (f *fakeUpstream) Fetch(ctx context.Context, cred Credential) (*Usage, error) {
	f.mu.Lock()
	f.calls[cred.ID]++
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

const testInterval = time.Minute

type harness struct {
	clock    *fakeClock
	upstream *fakeUpstream
	source   *fakeSource
	store    *Store
}

func newHarness(t *testing.T, creds ...Credential) *harness {
	t.Helper()
	h := &harness{
		clock:    &fakeClock{now: fixtureNow},
		upstream: newFakeUpstream(),
		source:   &fakeSource{creds: creds},
	}
	h.store = NewStore(h.source.list, h.upstream, func() time.Duration { return testInterval })
	h.store.now = h.clock.Now
	return h
}

// newPairHarness wires the two recorded accounts: "healthy" and "exhausted".
func newPairHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t,
		Credential{ID: "healthy", Index: "idx-healthy", Email: "healthy@example.test"},
		Credential{ID: "exhausted", Index: "idx-exhausted", Email: "exhausted@example.test"},
	)
	h.upstream.set("healthy", loadFixture(t, "usage-healthy.json"), nil)
	h.upstream.set("exhausted", loadFixture(t, "usage-weekly-exhausted.json"), nil)
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

func TestSnapshotBeforeFirstRefreshListsPendingAccountsWithoutFetching(t *testing.T) {
	h := newPairHarness(t)

	snap := h.store.Snapshot()

	if len(snap.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(snap.Accounts))
	}
	for _, account := range snap.Accounts {
		if account.State != StatePending || account.FetchedAt != nil || account.HasQuota != nil {
			t.Fatalf("account %s = %+v, want pending without data", account.ID, account)
		}
	}
	if snap.Summary.Accounts.Total != 2 || snap.Summary.Accounts.Pending != 2 || snap.Summary.WeeklyAllUsedPct != nil {
		t.Fatalf("summary = %+v, want 2 pending accounts and no percentages", snap.Summary)
	}
	if !snap.GeneratedAt.Equal(fixtureNow) {
		t.Fatalf("generated_at = %v, want %v", snap.GeneratedAt, fixtureNow)
	}
	if h.upstream.callCount("healthy")+h.upstream.callCount("exhausted") != 0 {
		t.Fatal("Snapshot fetched upstream, want memory-only read")
	}
}

func TestRefreshedPairReportsQuotaPerAccountAndSummary(t *testing.T) {
	h := newPairHarness(t)
	<-h.store.Tick(context.Background())

	snap := h.store.Snapshot()

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
	if sum.Accounts.Total != 2 || sum.Accounts.WithData != 2 || sum.Accounts.WithQuotaAny != 1 {
		t.Fatalf("summary accounts = %+v, want 2 total, 2 with data, 1 with quota", sum.Accounts)
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
	if fable.UsedPct != 23.5 || fable.RemainingPct != 45.5 || fable.AccountsWithQuota != 1 {
		t.Fatalf("scoped Fable = %+v, want used 23.5, remaining 45.5, 1 account with quota", fable)
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
	if _, known := h.store.HasQuota("missing", "claude-fable-5"); known {
		t.Fatal("HasQuota(missing) known = true, want false")
	}
}

func TestSummaryListsUnknownScopedModels(t *testing.T) {
	h := newHarness(t, Credential{ID: "a"})
	h.upstream.set("a", []byte(`{"limits":[
		{"kind":"weekly_all","percent":20,"scope":null},
		{"kind":"weekly_scoped","percent":30,"resets_at":null,"scope":{"model":{"display_name":"Nimbus Max"}}}
	]}`), nil)
	<-h.store.Tick(context.Background())

	scoped, ok := h.store.Snapshot().Summary.Scoped["Nimbus Max"]
	if !ok || scoped.UsedPct != 30 || scoped.RemainingPct != 70 || scoped.AccountsWithQuota != 1 {
		t.Fatalf("summary.scoped[Nimbus Max] = %+v (present %v), want used 30, remaining 70, 1 account", scoped, ok)
	}
}

func TestInFlightRefreshIsSingleFlightAndServesPreviousValue(t *testing.T) {
	h := newPairHarness(t)
	<-h.store.Tick(context.Background())

	h.clock.Advance(testInterval)
	h.upstream.set("healthy", loadFixture(t, "usage-weekly-exhausted.json"), nil)
	started := h.upstream.hold("healthy")
	done := h.store.Tick(context.Background())
	<-started
	h.clock.Advance(testInterval)
	<-h.store.Tick(context.Background())

	if got := h.upstream.callCount("healthy"); got != 2 {
		t.Fatalf("healthy fetches = %d, want 2 (initial + one in flight, no duplicate)", got)
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

func TestRefreshOutageMarksPreviousValueStale(t *testing.T) {
	h := newPairHarness(t)
	<-h.store.Tick(context.Background())

	started := h.upstream.hold("healthy")
	h.clock.Advance(testInterval)
	done := h.store.Tick(context.Background())
	<-started
	h.clock.Advance(testInterval + time.Second)

	stale := accountByID(t, h.store.Snapshot(), "healthy")
	if !stale.Stale || stale.WeeklyAll == nil || stale.WeeklyAll.Percent != 9 {
		t.Fatalf("healthy with overdue refresh = %+v, want stale previous value", stale)
	}
	h.upstream.release("healthy")
	<-done
}

func TestUpstreamErrorKeepsLastGoodValueAndOthersRefresh(t *testing.T) {
	h := newPairHarness(t)
	<-h.store.Tick(context.Background())

	h.clock.Advance(testInterval)
	h.upstream.set("healthy", nil, errors.New("fetch Claude OAuth usage failed with status 429"))
	h.upstream.set("exhausted", loadFixture(t, "usage-healthy.json"), nil)
	<-h.store.Tick(context.Background())

	snap := h.store.Snapshot()
	healthy := accountByID(t, snap, "healthy")
	if healthy.State != StateError || !healthy.Stale || healthy.Error == "" || healthy.WeeklyAll.Percent != 9 {
		t.Fatalf("healthy after error = %+v, want error state, stale, last good weekly_all 9", healthy)
	}
	if !healthy.FetchedAt.Equal(fixtureNow) {
		t.Fatalf("healthy fetched_at = %v, want last good fetch %v", healthy.FetchedAt, fixtureNow)
	}
	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); !ok || !known {
		t.Fatalf("HasQuota(healthy) after error = %v/%v, want last good value true/true", ok, known)
	}
	exhausted := accountByID(t, snap, "exhausted")
	if exhausted.State != StateOK || exhausted.WeeklyAll.Percent != 9 {
		t.Fatalf("exhausted = %+v, want refreshed despite the other account failing", exhausted)
	}
}

func TestHungFetchDoesNotBlockOtherCredentials(t *testing.T) {
	h := newPairHarness(t)
	started := h.upstream.hold("healthy")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.store.Tick(ctx)
	<-started
	// Spins until the other credential lands; a regression hangs until go test's timeout.
	for accountByID(t, h.store.Snapshot(), "exhausted").State != StateOK {
		runtime.Gosched()
	}
	if accountByID(t, h.store.Snapshot(), "healthy").State != StatePending {
		t.Fatal("healthy left pending state while its fetch hung")
	}
}

func TestDisabledCredentialIsListedButNeitherFetchedNorSummarised(t *testing.T) {
	h := newPairHarness(t)
	h.source.update("exhausted", func(c *Credential) { c.Disabled = true })
	<-h.store.Tick(context.Background())

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

func TestQuotaCooldownBlocksAndTriggersEarlyRefresh(t *testing.T) {
	h := newPairHarness(t)
	<-h.store.Tick(context.Background())

	h.clock.Advance(earlyRefreshGap)
	until := h.clock.Now().Add(10 * time.Minute)
	h.source.update("healthy", func(c *Credential) { c.CooldownUntil = until })
	<-h.store.Tick(context.Background())

	if got := h.upstream.callCount("healthy"); got != 2 {
		t.Fatalf("healthy fetches after cooldown = %d, want early refresh (2)", got)
	}
	if got := h.upstream.callCount("exhausted"); got != 1 {
		t.Fatalf("exhausted fetches = %d, want 1 (not due)", got)
	}
	if ok, known := h.store.HasQuota("healthy", "claude-fable-5"); ok || !known {
		t.Fatalf("HasQuota(cooled healthy) = %v/%v, want false/true", ok, known)
	}
	account := accountByID(t, h.store.Snapshot(), "healthy")
	if account.HasQuota.Any || account.CooldownUntil == nil || !account.CooldownUntil.Equal(until) {
		t.Fatalf("cooled account = %+v, want no quota and cooldown_until %v", account, until)
	}
}
