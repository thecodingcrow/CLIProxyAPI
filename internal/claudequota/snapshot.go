package claudequota

import (
	"math"
	"time"
)

// Account states.
const (
	StatePending  = "pending"
	StateOK       = "ok"
	StateDisabled = "disabled"
)

// AveragingMean names how summary percentages combine accounts: a plain mean
// over accounts with data, assuming equal plan capacity per account.
const AveragingMean = "mean"

// Snapshot is the observed quota view of every Claude OAuth credential.
type Snapshot struct {
	// Enabled is always true: the source is passive.
	Enabled     bool      `json:"enabled"`
	GeneratedAt time.Time `json:"generated_at"`
	Accounts    []Account `json:"accounts"`
	Summary     Summary   `json:"summary"`
}

// Account is one credential's observed limits.
type Account struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name,omitempty"`
	Label     string `json:"label,omitempty"`
	Email     string `json:"email,omitempty"`
	Disabled  bool   `json:"disabled"`
	State     string `json:"state"`
	// ObservedAt is the newest response any reported window came from.
	ObservedAt *time.Time `json:"observed_at"`
	// Stale is set once a window outlived its reset; that window counts as reset.
	Stale         bool             `json:"stale"`
	CooldownUntil *time.Time       `json:"cooldown_until,omitempty"`
	Session       *Limit           `json:"session"`
	WeeklyAll     *Limit           `json:"weekly_all"`
	WeeklyScoped  map[string]Limit `json:"weekly_scoped"`
	// HasQuota is nil until a window is observed.
	HasQuota *HasQuota `json:"has_quota"`
}

// HasQuota is the derived availability of one account.
type HasQuota struct {
	// Any covers models without a scoped limit: session and weekly_all not
	// exhausted and no credential-wide quota cooldown.
	Any bool `json:"any"`
	// Scoped additionally requires that scoped limit not exhausted, keyed by display name.
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

func account(cred Credential, now time.Time) Account {
	account := Account{
		ID:        cred.ID,
		AuthIndex: cred.Index,
		Name:      cred.Name,
		Label:     cred.Label,
		Email:     cred.Email,
		Disabled:  cred.Disabled,
	}
	if cred.CooldownUntil.After(now) {
		until := cred.CooldownUntil
		account.CooldownUntil = &until
	}
	usage := UsageFrom(cred.Observations)
	switch {
	case cred.Disabled:
		account.State = StateDisabled
	case usage == nil:
		account.State = StatePending
	default:
		account.State = StateOK
	}
	if usage == nil {
		return account
	}
	observedAt := usage.observedAt()
	account.ObservedAt = &observedAt
	account.Stale = usage.stale(now)
	account.Session = usage.Session
	account.WeeklyAll = usage.WeeklyAll
	account.WeeklyScoped = usage.Scoped

	unscoped := !cooledDown(cred, now, nil) && usage.HasQuota("", now)
	hasQuota := &HasQuota{Any: unscoped, Scoped: make(map[string]bool, len(usage.Scoped))}
	for name, limit := range usage.Scoped {
		modelCooled := cooledDown(cred, now, func(modelID string) bool { return ScopeMatchesModel(name, modelID) })
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
		if account.State == StatePending {
			summary.Accounts.Pending++
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
		// Ended windows count as 0%; the account keeps the raw reading and stale.
		weeklyPct := 0.0
		if account.WeeklyAll != nil {
			weeklyPct = account.WeeklyAll.usedPercent(now)
			weekly = append(weekly, weeklyPct)
		}
		if account.Session != nil {
			session = append(session, account.Session.usedPercent(now))
		}
		for name, limit := range account.WeeklyScoped {
			used := limit.usedPercent(now)
			scopedUsed[name] = append(scopedUsed[name], used)
			scopedRemaining[name] = append(scopedRemaining[name], math.Max(0, 100-math.Max(used, weeklyPct)))
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
