package claudequota

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// DefaultRefreshInterval applies when the configured interval is zero.
	DefaultRefreshInterval = 10 * time.Minute
	// MinRefreshInterval is the floor for any configured interval.
	MinRefreshInterval = 5 * time.Minute
	// MaxBackoff caps the retry delay after consecutive failures.
	MaxBackoff = time.Hour
	// backoffJitter is the maximum extra fraction added to a backoff delay.
	backoffJitter = 0.2
	// fetchSpacing separates consecutive fetches inside one serial round.
	fetchSpacing = 2 * time.Second
	// tickEvery bounds how late a due poll starts.
	tickEvery = 30 * time.Second
	// staleAfterIntervals marks a value stale once it is this many intervals old.
	staleAfterIntervals = 2
)

// ErrCredentialRejected marks a fetch the upstream refused for the credential
// itself (401/403). Polling for it pauses until its access token changes.
var ErrCredentialRejected = errors.New("credential rejected by usage endpoint")

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

// Settings is the live polling configuration, read on every tick and read.
type Settings struct {
	Enabled bool
	// Interval is clamped to [MinRefreshInterval, ∞); zero means DefaultRefreshInterval.
	Interval time.Duration
}

// Options supplies the clock, pause and randomness sources; zero values use
// the wall clock, a real timer and math/rand.
type Options struct {
	Now func() time.Time
	// Pause waits d between serial fetches; false aborts the round.
	Pause func(ctx context.Context, d time.Duration) bool
	// Jitter returns a value in [0, 1) scaling the backoff jitter.
	Jitter func() float64
}

// Reader is the read side routing consumes. Both calls are memory-only.
type Reader interface {
	// HasQuota reports whether authID may serve model by its last good usage
	// and its quota cooldowns as of the last sync. known is false when polling
	// is off or no usage was ever fetched; ok is then true, and callers must
	// never block routing on an unknown answer.
	HasQuota(authID, model string) (ok, known bool)
	Snapshot() Snapshot
}

type entry struct {
	cred      Credential
	usage     *Usage
	fetchedAt time.Time
	nextAt    time.Time
	failures  int
	err       string
	inflight  bool
	// rejectedToken is the access token the upstream refused; polling resumes
	// once the credential carries a different one.
	rejectedToken string
}

func (e *entry) pollable(now time.Time) bool {
	return !e.cred.Disabled && e.cred.AccessToken != "" && e.rejectedToken == "" && !now.Before(e.nextAt)
}

// Store caches usage per credential. Snapshot and HasQuota only read memory;
// Tick owns every upstream call: never before one full interval after a
// credential is first seen, one serial round at a time with spacing, and
// exponential backoff with jitter after failures.
type Store struct {
	source   Source
	fetcher  Fetcher
	settings func() Settings
	now      func() time.Time
	pause    func(ctx context.Context, d time.Duration) bool
	jitter   func() float64

	mu      sync.Mutex
	entries map[string]*entry
	running bool
}

var _ Reader = (*Store)(nil)

// NewStore builds a store; settings is read on every tick so configuration
// reloads apply without restart.
func NewStore(source Source, fetcher Fetcher, settings func() Settings, opts Options) *Store {
	s := &Store{
		source:   source,
		fetcher:  fetcher,
		settings: settings,
		now:      opts.Now,
		pause:    opts.Pause,
		jitter:   opts.Jitter,
		entries:  map[string]*entry{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.pause == nil {
		s.pause = sleepContext
	}
	if s.jitter == nil {
		s.jitter = rand.Float64
	}
	return s
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Store) currentSettings() (enabled bool, interval time.Duration) {
	if s.settings == nil {
		return false, 0
	}
	st := s.settings()
	interval = st.Interval
	if interval == 0 {
		interval = DefaultRefreshInterval
	}
	return st.Enabled, max(interval, MinRefreshInterval)
}

// Run ticks every tickEvery until ctx ends. It does not poll at start: the
// first poll of a credential waits one full interval.
func (s *Store) Run(ctx context.Context) {
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

// Tick syncs credentials and, unless a round is still running, starts one
// serial round over every due credential. Disabled polling drops the cache.
// The returned channel closes when the started round ends.
func (s *Store) Tick(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	enabled, interval := s.currentSettings()
	if !enabled || s.fetcher == nil {
		s.mu.Lock()
		s.entries = map[string]*entry{}
		s.mu.Unlock()
		close(done)
		return done
	}
	now := s.now()
	s.sync(now, interval)

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		close(done)
		return done
	}
	var due []Credential
	for _, e := range s.entries {
		if e.pollable(now) {
			due = append(due, e.cred)
		}
	}
	if len(due) == 0 {
		s.mu.Unlock()
		close(done)
		return done
	}
	s.running = true
	s.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })

	go func() {
		defer close(done)
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		for i, cred := range due {
			if i > 0 && !s.pause(ctx, fetchSpacing) {
				return
			}
			s.fetch(ctx, cred, interval)
		}
	}()
	return done
}

func (s *Store) fetch(ctx context.Context, cred Credential, interval time.Duration) {
	s.mu.Lock()
	e, ok := s.entries[cred.ID]
	if !ok {
		s.mu.Unlock()
		return
	}
	e.inflight = true
	s.mu.Unlock()

	usage, err := s.fetcher.Fetch(ctx, cred)

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e.inflight = false
	if s.entries[cred.ID] != e {
		return
	}
	if err == nil {
		e.usage, e.fetchedAt, e.err, e.failures = usage, now, "", 0
		e.nextAt = now.Add(interval)
		return
	}
	e.err = err.Error()
	e.failures++
	if errors.Is(err, ErrCredentialRejected) {
		e.rejectedToken = cred.AccessToken
		log.WithField("auth_id", cred.ID).Warn("claude quota polling paused until the access token changes")
		return
	}
	e.nextAt = now.Add(s.backoff(interval, e.failures))
	log.WithField("auth_id", cred.ID).Debugf("claude quota refresh failed: %v", err)
}

// backoff doubles interval per consecutive failure, adds up to backoffJitter,
// and caps at MaxBackoff.
func (s *Store) backoff(interval time.Duration, failures int) time.Duration {
	delay := interval
	for i := 0; i < failures && delay < MaxBackoff; i++ {
		delay *= 2
	}
	delay = time.Duration(float64(delay) * (1 + backoffJitter*s.jitter()))
	return min(delay, MaxBackoff)
}

// sync mirrors the source into entries: new credentials wait one interval,
// vanished ones are dropped, and a changed token lifts a rejection.
func (s *Store) sync(now time.Time, interval time.Duration) {
	if s.source == nil {
		return
	}
	creds := s.source()
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{}, len(creds))
	for _, cred := range creds {
		seen[cred.ID] = struct{}{}
		e, ok := s.entries[cred.ID]
		if !ok {
			s.entries[cred.ID] = &entry{cred: cred, nextAt: now.Add(interval)}
			continue
		}
		if e.rejectedToken != "" && cred.AccessToken != e.rejectedToken {
			e.rejectedToken = ""
			e.nextAt = now
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
	if enabled, _ := s.currentSettings(); !enabled {
		return true, false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, exists := s.entries[authID]
	if !exists || e.usage == nil {
		return true, false
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

// Snapshot implements Reader. With polling on it syncs credentials from the
// source (memory only) so new, removed and disabled credentials show at once.
func (s *Store) Snapshot() Snapshot {
	now := s.now()
	enabled, interval := s.currentSettings()
	if !enabled {
		return Snapshot{Enabled: false, GeneratedAt: now, Accounts: []Account{}, Summary: summarize(nil, now)}
	}
	s.sync(now, interval)
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
	return Snapshot{
		Enabled:                true,
		GeneratedAt:            now,
		RefreshIntervalSeconds: int(interval / time.Second),
		Accounts:               accounts,
		Summary:                summarize(accounts, now),
	}
}
