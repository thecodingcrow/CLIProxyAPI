package claudequota

import (
	"math"
	"time"
)

// Account states.
const (
	StatePending  = "pending"
	StateOK       = "ok"
	StateError    = "error"
	StateDisabled = "disabled"
)

// AveragingMean names how summary percentages combine accounts: a plain mean
// over accounts with data, assuming equal plan capacity per account.
const AveragingMean = "mean"

// Snapshot is the cached quota view of every Claude OAuth credential.
type Snapshot struct {
	// Enabled is false while polling is off; accounts are then empty.
	Enabled     bool      `json:"enabled"`
	GeneratedAt time.Time `json:"generated_at"`
	// RefreshIntervalSeconds is the effective poll interval; 0 while disabled.
	RefreshIntervalSeconds int       `json:"refresh_interval_seconds"`
	Accounts               []Account `json:"accounts"`
	Summary                Summary   `json:"summary"`
}

// Account is one credential's cached limits.
type Account struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name,omitempty"`
	Label     string `json:"label,omitempty"`
	Email     string `json:"email,omitempty"`
	Disabled  bool   `json:"disabled"`
	State     string `json:"state"`
	// FetchedAt is the time of the last successful fetch.
	FetchedAt *time.Time `json:"fetched_at"`
	// NextPollAt is when the next upstream poll is scheduled; nil while paused.
	NextPollAt *time.Time `json:"next_poll_at"`
	// Stale is set when the last attempt failed or the value outlived two intervals.
	Stale bool `json:"stale"`
	// Refreshing is set while a fetch for this credential is in flight.
	Refreshing    bool             `json:"refreshing"`
	Error         string           `json:"error,omitempty"`
	CooldownUntil *time.Time       `json:"cooldown_until,omitempty"`
	Session       *Limit           `json:"session"`
	WeeklyAll     *Limit           `json:"weekly_all"`
	WeeklyScoped  map[string]Limit `json:"weekly_scoped"`
	// HasQuota is nil until the first successful fetch.
	HasQuota *HasQuota `json:"has_quota"`
}

// HasQuota is the derived availability of one account.
type HasQuota struct {
	// Any covers models without a scoped limit: session and weekly_all below
	// 100% and no credential-wide quota cooldown.
	Any bool `json:"any"`
	// Scoped additionally requires that scoped limit below 100%, keyed by display name.
	Scoped map[string]bool `json:"scoped"`
}

// Summary aggregates enabled accounts; disabled ones are only counted.
type Summary struct {
	Accounts         SummaryAccounts          `json:"accounts"`
	Averaging        string                   `json:"averaging"`
	WeeklyAllUsedPct *float64                 `json:"weekly_all_used_pct"`
	SessionUsedPct   *float64                 `json:"session_used_pct"`
	Scoped           map[string]ScopedSummary `json:"scoped"`
	// SoonestReset is the earliest future reset across all limits of enabled accounts.
	SoonestReset *time.Time `json:"soonest_reset"`
	GeneratedAt  time.Time  `json:"generated_at"`
}

// SummaryAccounts counts accounts by state.
type SummaryAccounts struct {
	// Total counts enabled accounts.
	Total    int `json:"total"`
	WithData int `json:"with_data"`
	Pending  int `json:"pending"`
	Errored  int `json:"errored"`
	Stale    int `json:"stale"`
	// WithQuotaAny counts accounts that can serve models without a scoped limit.
	WithQuotaAny int `json:"with_quota_any"`
	Disabled     int `json:"disabled"`
}

// ScopedSummary aggregates one model-scoped weekly limit.
type ScopedSummary struct {
	// UsedPct is the mean scoped percent over accounts reporting the scope.
	UsedPct float64 `json:"used_pct"`
	// RemainingPct is the mean of 100 - max(scoped, weekly_all): headroom the
	// account can actually spend, so used + remaining may be below 100.
	RemainingPct float64 `json:"remaining_pct"`
	// AccountsWithQuota counts accounts with data that can serve the model now.
	AccountsWithQuota int `json:"accounts_with_quota"`
	// Accounts counts accounts reporting the scope.
	Accounts int `json:"accounts"`
}

func (e *entry) account(now time.Time, interval time.Duration) Account {
	account := Account{
		ID:         e.cred.ID,
		AuthIndex:  e.cred.Index,
		Name:       e.cred.Name,
		Label:      e.cred.Label,
		Email:      e.cred.Email,
		Disabled:   e.cred.Disabled,
		Refreshing: e.inflight,
		Error:      e.err,
	}
	if !e.cred.Disabled && e.rejectedToken == "" {
		nextAt := e.nextAt
		account.NextPollAt = &nextAt
	}
	if e.cred.CooldownUntil.After(now) {
		until := e.cred.CooldownUntil
		account.CooldownUntil = &until
	}
	switch {
	case e.cred.Disabled:
		account.State = StateDisabled
	case e.err != "":
		account.State = StateError
	case e.usage == nil:
		account.State = StatePending
	default:
		account.State = StateOK
	}
	if e.usage == nil {
		account.Stale = e.err != ""
		return account
	}
	fetchedAt := e.fetchedAt
	account.FetchedAt = &fetchedAt
	account.Stale = e.err != "" || now.Sub(e.fetchedAt) > staleAfterIntervals*interval
	account.Session = e.usage.Session
	account.WeeklyAll = e.usage.WeeklyAll
	account.WeeklyScoped = e.usage.Scoped

	unscoped := !cooledDown(e.cred, now, nil) && e.usage.HasQuota("", now)
	hasQuota := &HasQuota{Any: unscoped, Scoped: make(map[string]bool, len(e.usage.Scoped))}
	for name, limit := range e.usage.Scoped {
		modelCooled := cooledDown(e.cred, now, func(modelID string) bool { return ScopeMatchesModel(name, modelID) })
		hasQuota.Scoped[name] = unscoped && !modelCooled && !limit.exhausted(now)
	}
	account.HasQuota = hasQuota
	return account
}

func summarize(accounts []Account, now time.Time) Summary {
	summary := Summary{Averaging: AveragingMean, Scoped: map[string]ScopedSummary{}, GeneratedAt: now}
	var weekly, session []float64
	scopedUsed := map[string][]float64{}
	scopedRemaining := map[string][]float64{}
	var withData []Account
	for _, account := range accounts {
		if account.Disabled {
			summary.Accounts.Disabled++
			continue
		}
		summary.Accounts.Total++
		switch account.State {
		case StatePending:
			summary.Accounts.Pending++
		case StateError:
			summary.Accounts.Errored++
		}
		if account.Stale {
			summary.Accounts.Stale++
		}
		if account.HasQuota == nil {
			continue
		}
		withData = append(withData, account)
		summary.Accounts.WithData++
		if account.HasQuota.Any {
			summary.Accounts.WithQuotaAny++
		}
		weeklyPct := 0.0
		if account.WeeklyAll != nil {
			weeklyPct = account.WeeklyAll.Percent
			weekly = append(weekly, weeklyPct)
		}
		if account.Session != nil {
			session = append(session, account.Session.Percent)
		}
		for name, limit := range account.WeeklyScoped {
			scopedUsed[name] = append(scopedUsed[name], limit.Percent)
			scopedRemaining[name] = append(scopedRemaining[name], math.Max(0, 100-math.Max(limit.Percent, weeklyPct)))
		}
		for _, limit := range accountLimits(account) {
			if limit.ResetsAt != nil && limit.ResetsAt.After(now) && (summary.SoonestReset == nil || limit.ResetsAt.Before(*summary.SoonestReset)) {
				reset := *limit.ResetsAt
				summary.SoonestReset = &reset
			}
		}
	}
	summary.WeeklyAllUsedPct = mean(weekly)
	summary.SessionUsedPct = mean(session)
	for name, used := range scopedUsed {
		scoped := ScopedSummary{UsedPct: *mean(used), RemainingPct: *mean(scopedRemaining[name]), Accounts: len(used)}
		for _, account := range withData {
			if available, reported := account.HasQuota.Scoped[name]; available || (!reported && account.HasQuota.Any) {
				scoped.AccountsWithQuota++
			}
		}
		summary.Scoped[name] = scoped
	}
	return summary
}

func accountLimits(account Account) []Limit {
	limits := make([]Limit, 0, 2+len(account.WeeklyScoped))
	if account.Session != nil {
		limits = append(limits, *account.Session)
	}
	if account.WeeklyAll != nil {
		limits = append(limits, *account.WeeklyAll)
	}
	for _, limit := range account.WeeklyScoped {
		limits = append(limits, limit)
	}
	return limits
}

// mean rounds to two decimals; nil when values is empty.
func mean(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	result := math.Round(total/float64(len(values))*100) / 100
	return &result
}
