package claudequota

import (
	"context"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// DefaultRefreshInterval applies when the configured interval is zero.
	DefaultRefreshInterval = 60 * time.Second
	// tickEvery bounds how late a due refresh or a new cooldown is noticed.
	tickEvery = 5 * time.Second
	// earlyRefreshGap is the minimum age of the last attempt before a new
	// quota cooldown pulls a refresh forward.
	earlyRefreshGap = 15 * time.Second
	// staleAfterIntervals marks a value stale once it is this many intervals old.
	staleAfterIntervals = 2
)

// Credential is the routing-relevant view of one Claude OAuth credential.
type Credential struct {
	ID       string
	Index    string
	Name     string
	Label    string
	Email    string
	Disabled bool
	// AccessToken is the bearer the fetcher sends; never serialised or logged.
	AccessToken string
	ProxyURL    string
	// CooldownUntil is a credential-wide quota cooldown; zero when none.
	CooldownUntil time.Time
	// ModelCooldowns holds model-scoped quota cooldowns keyed by model ID.
	ModelCooldowns map[string]time.Time
}

// Source lists the current Claude OAuth credentials from memory. It must not
// perform network calls.
type Source func() []Credential

// Fetcher retrieves one credential's usage from upstream.
type Fetcher interface {
	Fetch(ctx context.Context, cred Credential) (*Usage, error)
}

// Reader is the read side routing consumes. Both calls are memory-only.
type Reader interface {
	// HasQuota reports whether authID may serve model by its last good usage
	// and its quota cooldowns as of the last sync. known is false when no usage
	// was ever fetched for authID.
	HasQuota(authID, model string) (ok, known bool)
	Snapshot() Snapshot
}

type entry struct {
	cred      Credential
	usage     *Usage
	fetchedAt time.Time
	attemptAt time.Time
	err       string
	inflight  bool
	// cooldownBumped records a new quota cooldown not yet answered by a refresh.
	cooldownBumped bool
}

// Store caches usage per credential. Snapshot and HasQuota only read memory;
// Tick (driven by Run) owns every upstream call, at most one per credential at
// a time, and a failing or hung credential never delays the others.
type Store struct {
	source   Source
	fetcher  Fetcher
	interval func() time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

var _ Reader = (*Store)(nil)

// NewStore builds a store. interval is read on every tick so configuration
// reloads apply without restart; zero means DefaultRefreshInterval and a
// negative value stops upstream refreshes.
func NewStore(source Source, fetcher Fetcher, interval func() time.Duration) *Store {
	return &Store{
		source:   source,
		fetcher:  fetcher,
		interval: interval,
		now:      time.Now,
		entries:  map[string]*entry{},
	}
}

func (s *Store) refreshInterval() time.Duration {
	if s.interval == nil {
		return DefaultRefreshInterval
	}
	interval := s.interval()
	if interval == 0 {
		return DefaultRefreshInterval
	}
	return interval
}

// Run refreshes immediately, then every tickEvery until ctx ends.
func (s *Store) Run(ctx context.Context) {
	s.Tick(ctx)
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick syncs credentials from the source and starts a background fetch for
// every enabled credential that is due and not already in flight. The returned
// channel closes once the fetches started by this tick finish.
func (s *Store) Tick(ctx context.Context) <-chan struct{} {
	s.sync()
	done := make(chan struct{})
	interval := s.refreshInterval()
	if interval < 0 || s.fetcher == nil {
		close(done)
		return done
	}
	now := s.now()
	var due []Credential
	s.mu.Lock()
	for _, e := range s.entries {
		if e.cred.Disabled || e.inflight || !e.due(now, interval) {
			continue
		}
		e.inflight = true
		e.attemptAt = now
		e.cooldownBumped = false
		due = append(due, e.cred)
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, cred := range due {
		wg.Add(1)
		go func(cred Credential) {
			defer wg.Done()
			s.fetch(ctx, cred)
		}(cred)
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

func (e *entry) due(now time.Time, interval time.Duration) bool {
	if e.attemptAt.IsZero() {
		return true
	}
	age := now.Sub(e.attemptAt)
	return age >= interval || (e.cooldownBumped && age >= earlyRefreshGap)
}

func (s *Store) fetch(ctx context.Context, cred Credential) {
	usage, err := s.fetcher.Fetch(ctx, cred)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[cred.ID]
	if !ok {
		return
	}
	e.inflight = false
	if err != nil {
		e.err = err.Error()
		log.WithField("auth_id", cred.ID).Debugf("claude quota refresh failed: %v", err)
		return
	}
	e.usage = usage
	e.fetchedAt = now
	e.err = ""
}

// sync mirrors the source into entries: new credentials start pending,
// vanished ones are dropped, and a cooldown that moves later flags an early
// refresh.
func (s *Store) sync() {
	if s.source == nil {
		return
	}
	creds := s.source()
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(creds))
	for _, cred := range creds {
		seen[cred.ID] = struct{}{}
		e, ok := s.entries[cred.ID]
		if !ok {
			s.entries[cred.ID] = &entry{cred: cred}
			continue
		}
		if cred.CooldownUntil.After(now) && cred.CooldownUntil.After(e.cred.CooldownUntil) {
			e.cooldownBumped = true
		}
		e.cred = cred
	}
	for id := range s.entries {
		if _, ok := seen[id]; !ok {
			delete(s.entries, id)
		}
	}
}

// HasQuota implements Reader.
func (s *Store) HasQuota(authID, model string) (ok, known bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, exists := s.entries[authID]
	if !exists || e.usage == nil {
		return false, false
	}
	if cooledDown(e.cred, now, func(id string) bool { return id == model }) {
		return false, true
	}
	return e.usage.HasQuota(model, now), true
}

// cooledDown reports a credential-wide cooldown, or a model cooldown whose
// model ID satisfies matches.
func cooledDown(cred Credential, now time.Time, matches func(modelID string) bool) bool {
	if cred.CooldownUntil.After(now) {
		return true
	}
	if matches == nil {
		return false
	}
	for modelID, until := range cred.ModelCooldowns {
		if until.After(now) && matches(modelID) {
			return true
		}
	}
	return false
}

// Snapshot implements Reader. It syncs credentials from the source (memory
// only) so new, removed and disabled credentials show without waiting a tick.
func (s *Store) Snapshot() Snapshot {
	s.sync()
	now := s.now()
	interval := s.refreshInterval()
	s.mu.Lock()
	accounts := make([]Account, 0, len(s.entries))
	for _, e := range s.entries {
		accounts = append(accounts, e.account(now, interval))
	}
	s.mu.Unlock()
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Name != accounts[j].Name {
			return accounts[i].Name < accounts[j].Name
		}
		return accounts[i].ID < accounts[j].ID
	})
	seconds := int(interval / time.Second)
	if interval < 0 {
		seconds = -1
	}
	return Snapshot{
		GeneratedAt:            now,
		RefreshIntervalSeconds: seconds,
		Accounts:               accounts,
		Summary:                summarize(accounts, now),
	}
}
