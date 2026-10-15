package claudequota

import (
	"sort"
	"time"
)

// Credential is the routing-relevant view of one Claude OAuth credential.
type Credential struct {
	ID       string
	Index    string
	Name     string
	Label    string
	Email    string
	Disabled bool
	// CooldownUntil is a credential-wide quota cooldown; zero when none.
	CooldownUntil time.Time
	// ModelCooldowns holds model-scoped quota cooldowns keyed by model ID.
	ModelCooldowns map[string]time.Time
	// Observations are the quota header snapshots stored on the credential.
	Observations []Observation
}

// Source lists the current Claude OAuth credentials from memory.
type Source func() []Credential

// Lookup returns one Claude OAuth credential from memory; false when absent or not one.
type Lookup func(id string) (Credential, bool)

// Reader is the read side routing consumes. Every call is memory-only.
type Reader interface {
	// Status judges authID for model by its observed windows and quota cooldowns.
	Status(authID, model string) Status
	Snapshot() Snapshot
}

// Status is one credential's standing for one model.
type Status struct {
	// Known is false while the credential has no observed window; callers
	// must never block routing on an unknown answer.
	Known bool
	// OK reports quota for the model; always true when Known is false.
	OK bool
	// ResetAt is when the last window or cooldown blocking the model ends; zero
	// when OK or when a blocking window reports no reset time.
	ResetAt time.Time
	// HeadroomPct is the percent left in the weekly window that caps the
	// model; set only when Known and OK.
	HeadroomPct float64
	// DrainBy is when that capping window resets; zero when unreported.
	DrainBy time.Time
}

// Store derives quota from the headers each credential already carries; it
// holds no state and never calls upstream.
type Store struct {
	list   Source
	lookup Lookup
	now    func() time.Time
}

var _ Reader = (*Store)(nil)

// NewStore builds a store; a nil now uses the wall clock.
func NewStore(list Source, lookup Lookup, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{list: list, lookup: lookup, now: now}
}

// Status implements Reader.
func (s *Store) Status(authID, model string) Status {
	cred, ok := s.lookup(authID)
	if !ok {
		return Status{OK: true}
	}
	usage := UsageFrom(cred.Observations)
	if usage == nil {
		return Status{OK: true}
	}
	now := s.now()
	blocked, until := usage.blockedUntil(model, now)
	if cooled := cooldownUntil(cred, now, func(id string) bool { return id == model }); !cooled.IsZero() {
		// A window without reset time keeps the reset unknown.
		if !blocked || (!until.IsZero() && cooled.After(until)) {
			until = cooled
		}
		blocked = true
	}
	if !blocked {
		headroom, drainBy := usage.headroom(model, now)
		return Status{Known: true, OK: true, HeadroomPct: headroom, DrainBy: drainBy}
	}
	return Status{Known: true, ResetAt: until}
}

// cooledDown reports a credential-wide cooldown, or a model cooldown whose
// model ID satisfies matches.
func cooledDown(cred Credential, now time.Time, matches func(modelID string) bool) bool {
	return !cooldownUntil(cred, now, matches).IsZero()
}

// cooldownUntil returns the latest active cooldown end among the
// credential-wide one and model cooldowns whose model ID satisfies matches;
// zero when none is active.
func cooldownUntil(cred Credential, now time.Time, matches func(modelID string) bool) time.Time {
	var until time.Time
	if cred.CooldownUntil.After(now) {
		until = cred.CooldownUntil
	}
	if matches == nil {
		return until
	}
	for modelID, end := range cred.ModelCooldowns {
		if end.After(now) && end.After(until) && matches(modelID) {
			until = end
		}
	}
	return until
}

// Snapshot implements Reader.
func (s *Store) Snapshot() Snapshot {
	now := s.now()
	creds := s.list()
	accounts := make([]Account, 0, len(creds))
	for _, cred := range creds {
		accounts = append(accounts, account(cred, now))
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Name != accounts[j].Name {
			return accounts[i].Name < accounts[j].Name
		}
		return accounts[i].ID < accounts[j].ID
	})
	return Snapshot{Enabled: true, GeneratedAt: now, Accounts: accounts, Summary: summarize(accounts, now)}
}
