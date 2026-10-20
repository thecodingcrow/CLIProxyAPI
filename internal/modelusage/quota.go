package modelusage

import (
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
)

const (
	quotaProvider = "claude"
	quotaWindow   = 7 * 24 * time.Hour
)

// Quota prices the pooled weekly quota per USD of API-equivalent cost.
type Quota struct {
	Provider string `json:"provider"`
	// Accounts counts enabled accounts with quota data.
	Accounts   int          `json:"accounts"`
	Pooling    string       `json:"pooling"`
	WeeklyAll  *QuotaWindow `json:"weekly_all,omitempty"`
	Fable      *QuotaWindow `json:"fable,omitempty"`
	ObservedAt time.Time    `json:"observed_at"`
	Stale      bool         `json:"stale"`
}

// QuotaWindow is one weekly window across accounts, each over its own window.
type QuotaWindow struct {
	// UsedPct is the pooled mean.
	UsedPct float64 `json:"used_pct"`
	// CostUSDMicros sums the listed accounts' cost, each since its own window start.
	CostUSDMicros int64 `json:"cost_usd_micros"`
	// PctPerUSD is the listed accounts' used percent summed, over all pooled accounts, per USD of their cost.
	PctPerUSD float64        `json:"pct_per_usd"`
	Accounts  []QuotaAccount `json:"accounts"`
	// Models names the rows in the window's scope; Fable only.
	Models []string `json:"models,omitempty"`
}

// QuotaAccount is one account with proxy cost in its running window.
type QuotaAccount struct {
	AuthIndex     string    `json:"auth_index"`
	UsedPct       float64   `json:"used_pct"`
	WindowStart   time.Time `json:"window_start"`
	ResetsAt      time.Time `json:"resets_at"`
	CostUSDMicros int64     `json:"cost_usd_micros"`
}

// accountKey is one credential's cost series: all claude models, or only the Fable scope.
type accountKey struct {
	AuthIndex string
	Fable     bool
}

func inFableScope(model string) bool {
	return claudequota.ScopeMatchesModel(claudequota.ScopeFable, model)
}

// addAccountCostLocked keeps the priced claude cost per credential; other providers have no plan quota.
func (a *Aggregator) addAccountCostLocked(key Key, c Counters, authIndex string, currentID int64) {
	if authIndex == "" || key.Provider != quotaProvider || c.CostUSDMicros <= 0 {
		return
	}
	cost := Counters{CostUSDMicros: c.CostUSDMicros}
	a.accountSeriesLocked(accountKey{AuthIndex: authIndex}).add(currentID, cost, currentID)
	if inFableScope(key.Model) {
		a.accountSeriesLocked(accountKey{AuthIndex: authIndex, Fable: true}).add(currentID, cost, currentID)
	}
}

// accountSeriesLocked drops the least recently active credential at the key cap: counts there are only a rate input.
func (a *Aggregator) accountSeriesLocked(key accountKey) *series {
	if s, ok := a.accounts[key]; ok {
		return s
	}
	for len(a.accounts) >= a.maxKeys {
		var oldest accountKey
		var victim *series
		for k, s := range a.accounts {
			if victim == nil || s.lastID < victim.lastID {
				oldest, victim = k, s
			}
		}
		delete(a.accounts, oldest)
	}
	s := &series{}
	a.accounts[key] = s
	return s
}

// quotaLocked builds the quota block and sets each row's quota share; nil without pool data.
func (a *Aggregator) quotaLocked(pool *claudequota.Pool, currentID int64, rows []Row) *Quota {
	if pool == nil || pool.Accounts == 0 {
		return nil
	}
	q := &Quota{Provider: quotaProvider, Accounts: pool.Accounts, Pooling: claudequota.AveragingMean, ObservedAt: pool.ObservedAt.UTC().Truncate(time.Second), Stale: pool.Stale}
	q.WeeklyAll = a.windowLocked(pool.WeeklyAll, false, currentID)
	q.Fable = a.windowLocked(pool.Fable, true, currentID)
	if q.Fable != nil {
		q.Fable.Models = a.fableModelsLocked()
	}
	for i := range rows {
		row := &rows[i]
		if row.Provider != quotaProvider {
			continue
		}
		usd := float64(row.CostUSDMicros) / 1e6
		if q.WeeklyAll != nil {
			pct := usd * q.WeeklyAll.PctPerUSD
			row.QuotaWeeklyPct = &pct
		}
		if q.Fable != nil && inFableScope(row.Model) {
			pct := usd * q.Fable.PctPerUSD
			row.QuotaFablePct = &pct
		}
	}
	return q
}

// windowLocked sums each running account window's cost. Accounts without cost there spent only outside
// the proxy and leave the sum, but the pool still divides by every account: the rate stays a share of the pool.
func (a *Aggregator) windowLocked(pooled claudequota.Pooled, fable bool, currentID int64) *QuotaWindow {
	if pooled.Pooled == 0 {
		return nil
	}
	w := &QuotaWindow{UsedPct: pooled.UsedPct, Accounts: []QuotaAccount{}}
	var used float64
	for _, win := range pooled.Windows {
		start := win.ResetsAt.Add(-quotaWindow)
		s, ok := a.accounts[accountKey{AuthIndex: win.AuthIndex, Fable: fable}]
		if !ok {
			continue
		}
		cost := s.sumSince(start, currentID).CostUSDMicros
		if cost <= 0 {
			continue
		}
		used += win.UsedPct
		w.CostUSDMicros += cost
		w.Accounts = append(w.Accounts, QuotaAccount{
			AuthIndex: win.AuthIndex, UsedPct: win.UsedPct, WindowStart: start.UTC().Truncate(time.Second), ResetsAt: win.ResetsAt.UTC().Truncate(time.Second), CostUSDMicros: cost,
		})
	}
	if w.CostUSDMicros == 0 {
		return nil
	}
	w.PctPerUSD = used / float64(pooled.Pooled) / (float64(w.CostUSDMicros) / 1e6)
	return w
}

// sumSince counts buckets starting at or after since, up to currentID.
func (s *series) sumSince(since time.Time, currentID int64) Counters {
	fromID := (since.Unix() + BucketSeconds - 1) / BucketSeconds
	var c Counters
	for _, b := range s.buckets {
		if b.ID >= fromID && b.ID <= currentID {
			c.add(b.Counters)
		}
	}
	return c
}

func (a *Aggregator) fableModelsLocked() []string {
	seen := map[string]bool{}
	for key := range a.series {
		if key.Provider == quotaProvider && inFableScope(key.Model) {
			seen[key.Model] = true
		}
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}
