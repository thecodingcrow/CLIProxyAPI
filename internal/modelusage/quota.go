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

// Rate methods: measured pairs each account's utilization change with its own cost; rough spreads the pooled
// percent over all claude cost since the earliest running window start, before a measured rate qualifies.
const (
	MethodMeasured = "measured"
	MethodRough    = "rough"
)

// QuotaWindow is one weekly window across accounts, rated by Method.
type QuotaWindow struct {
	Method string `json:"method"`
	// WindowStart is where a rough rate's cost begins; measured rates start per account.
	WindowStart *time.Time `json:"window_start,omitempty"`
	// UsedPct is the pooled mean.
	UsedPct float64 `json:"used_pct"`
	// CostUSDMicros is the measured accounts' cost since their baselines, or rough: all claude cost since WindowStart.
	CostUSDMicros int64 `json:"cost_usd_micros"`
	// PctPerUSD is measured: the listed accounts' utilization change summed, over all pooled accounts, per USD of
	// their cost; rough: UsedPct per USD of CostUSDMicros.
	PctPerUSD float64 `json:"pct_per_usd"`
	// AccountsInRate counts the listed accounts.
	AccountsInRate int            `json:"accounts_in_rate"`
	Accounts       []QuotaAccount `json:"accounts"`
	// Models names the rows in the window's scope; Fable only.
	Models []string `json:"models,omitempty"`
}

// QuotaAccount is one account in the rate; the cost and baseline fields are measured only.
type QuotaAccount struct {
	AuthIndex   string    `json:"auth_index"`
	UsedPct     float64   `json:"used_pct"`
	WindowStart time.Time `json:"window_start"`
	ResetsAt    time.Time `json:"resets_at"`
	// CostUSDMicros is the cost since BaselineAt.
	CostUSDMicros   *int64     `json:"cost_usd_micros,omitempty"`
	BaselineAt      *time.Time `json:"baseline_at,omitempty"`
	BaselineUsedPct *float64   `json:"baseline_used_pct,omitempty"`
	// Exact: the baseline is the window start, so the whole window's utilization pairs with its cost.
	Exact bool `json:"exact"`
}

// accountKey is one credential's cost series: all claude models, or only the Fable scope.
type accountKey struct {
	AuthIndex string
	Fable     bool
}

func inFableScope(model string) bool {
	return claudequota.ScopeMatchesModel(claudequota.ScopeFable, model)
}

// minDeltaPct is the smallest utilization change any rate uses: readings move in ~1-point steps, over cents right after a rollover.
const minDeltaPct = 5

// account is one credential's cost: 7 days of buckets, the cumulative total since costSince, and the
// baseline its quota window is rated from.
type account struct {
	series
	base *baseline
}

// baseline pairs one utilization reading with the account's cumulative cost at that moment.
type baseline struct {
	WindowStart time.Time `json:"window_start"`
	ResetsAt    time.Time `json:"resets_at"`
	UsedPct     float64   `json:"used_pct"`
	// CostUSDMicros is the account's cumulative cost at At.
	CostUSDMicros int64     `json:"cost_usd_micros"`
	At            time.Time `json:"at"`
	Exact         bool      `json:"exact"`
}

// addAccountCostLocked keeps the priced claude cost per credential; other providers have no plan quota.
func (a *Aggregator) addAccountCostLocked(key Key, c Counters, authIndex string, currentID int64) {
	if authIndex == "" || key.Provider != quotaProvider || c.CostUSDMicros <= 0 {
		return
	}
	cost := Counters{CostUSDMicros: c.CostUSDMicros}
	keys := []accountKey{{AuthIndex: authIndex}}
	if inFableScope(key.Model) {
		keys = append(keys, accountKey{AuthIndex: authIndex, Fable: true})
	}
	for _, k := range keys {
		acc := a.accountLocked(k)
		acc.add(currentID, cost, currentID)
		acc.total.add(cost)
	}
}

// accountLocked drops the least recently active credential at the key cap: counts there are only a rate input.
func (a *Aggregator) accountLocked(key accountKey) *account {
	if acc, ok := a.accounts[key]; ok {
		return acc
	}
	for len(a.accounts) >= a.maxKeys {
		var oldest accountKey
		var victim *account
		for k, acc := range a.accounts {
			if victim == nil || acc.lastID < victim.lastID {
				oldest, victim = k, acc
			}
		}
		delete(a.accounts, oldest)
	}
	acc := &account{}
	a.accounts[key] = acc
	return acc
}

// costAt is the cumulative cost before the first bucket starting at or after at.
func (acc *account) costAt(at time.Time, currentID int64) int64 {
	return acc.total.CostUSDMicros - acc.sumSince(at, currentID).CostUSDMicros
}

// sameWindow tolerates reset timestamps jittering between responses; a new window moves them by days.
func (b *baseline) sameWindow(resetsAt time.Time) bool {
	return b != nil && b.ResetsAt.Sub(resetsAt).Abs() <= time.Hour
}

// observeQuotaLocked keeps the first reading of each window that tracking did not cover from its start,
// paired with the cumulative cost including the request that carried it. Covered windows need no reading.
func (a *Aggregator) observeQuotaLocked(authIndex string, reading *claudequota.Usage, currentID int64) {
	if reading == nil {
		return
	}
	observe := func(key accountKey, limit *claudequota.Limit) {
		if limit == nil || limit.ResetsAt == nil {
			return
		}
		acc := a.accountLocked(key)
		if acc.base.sameWindow(*limit.ResetsAt) {
			return
		}
		start := limit.ResetsAt.Add(-quotaWindow)
		acc.base = nil
		if a.costSince.After(start) {
			acc.base = &baseline{WindowStart: start, ResetsAt: *limit.ResetsAt, UsedPct: limit.Percent,
				CostUSDMicros: acc.total.CostUSDMicros, At: limit.ObservedAt}
		}
	}
	observe(accountKey{AuthIndex: authIndex}, reading.WeeklyAll)
	if fable, ok := reading.Scoped[claudequota.ScopeFable]; ok {
		observe(accountKey{AuthIndex: authIndex, Fable: true}, &fable)
	}
}

// baselineFor is win's baseline without changing state: exact (0%, cost at the window start) when tracking
// covered the start, else the recorded first reading; false when there is neither.
func (a *Aggregator) baselineFor(acc *account, win claudequota.AccountWindow, currentID int64) (baseline, bool) {
	start := win.ResetsAt.Add(-quotaWindow)
	if !a.costSince.After(start) {
		return baseline{WindowStart: start, ResetsAt: win.ResetsAt, CostUSDMicros: acc.costAt(start, currentID), At: start, Exact: true}, true
	}
	if acc.base.sameWindow(win.ResetsAt) {
		return *acc.base, true
	}
	return baseline{}, false
}

// quotaLocked builds the quota block and sets each row's quota share; nil without pool data.
func (a *Aggregator) quotaLocked(pool *claudequota.Pool, currentID int64, rows []Row) *Quota {
	if pool == nil || pool.Accounts == 0 {
		return nil
	}
	q := &Quota{Provider: quotaProvider, Accounts: pool.Accounts, Pooling: claudequota.AveragingMean, ObservedAt: utcSecond(pool.ObservedAt), Stale: pool.Stale}
	q.WeeklyAll = a.windowLocked(pool.WeeklyAll, false, currentID)
	if q.WeeklyAll == nil {
		q.WeeklyAll = a.roughLocked(pool.WeeklyAll, false, currentID)
	}
	q.Fable = a.windowLocked(pool.Fable, true, currentID)
	if q.Fable == nil {
		q.Fable = a.roughLocked(pool.Fable, true, currentID)
	}
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

// windowLocked rates utilization change per USD since each baseline, as a share of the whole pool; an account
// with spend in the window but no usable change withholds it, since rows carry that spend.
func (a *Aggregator) windowLocked(pooled claudequota.Pooled, fable bool, currentID int64) *QuotaWindow {
	if pooled.Pooled == 0 {
		return nil
	}
	w := &QuotaWindow{Method: MethodMeasured, UsedPct: pooled.UsedPct, Accounts: []QuotaAccount{}}
	var used float64
	for _, win := range pooled.Windows {
		acc, ok := a.accounts[accountKey{AuthIndex: win.AuthIndex, Fable: fable}]
		if !ok {
			continue
		}
		b, ok := a.baselineFor(acc, win, currentID)
		deltaUsed, deltaCost := win.UsedPct-b.UsedPct, acc.total.CostUSDMicros-b.CostUSDMicros
		if !ok || deltaCost <= 0 || deltaUsed < minDeltaPct {
			// Rows carry this account's spend too; rating it at the others' rate would inflate every row.
			if acc.sumSince(maxTime(win.ResetsAt.Add(-quotaWindow), a.costSince), currentID).CostUSDMicros > 0 {
				return nil
			}
			continue
		}
		used += deltaUsed
		w.CostUSDMicros += deltaCost
		w.Accounts = append(w.Accounts, QuotaAccount{
			AuthIndex: win.AuthIndex, UsedPct: win.UsedPct, WindowStart: utcSecond(b.WindowStart), ResetsAt: utcSecond(win.ResetsAt),
			CostUSDMicros: &deltaCost, BaselineAt: ptrTime(utcSecond(b.At)), BaselineUsedPct: &b.UsedPct, Exact: b.Exact,
		})
	}
	if w.CostUSDMicros == 0 {
		return nil
	}
	w.AccountsInRate = len(w.Accounts)
	w.PctPerUSD = used / float64(pooled.Pooled) / (float64(w.CostUSDMicros) / 1e6)
	return w
}

func maxTime(x, y time.Time) time.Time {
	if x.After(y) {
		return x
	}
	return y
}

func utcSecond(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

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

// roughLocked spreads the pooled percent over all claude cost (every account, backfilled history too) since the
// earliest running window start; nil without a running window, minDeltaPct of utilization or cost.
func (a *Aggregator) roughLocked(pooled claudequota.Pooled, fable bool, currentID int64) *QuotaWindow {
	if len(pooled.Windows) == 0 || pooled.UsedPct < minDeltaPct {
		return nil
	}
	w := &QuotaWindow{Method: MethodRough, UsedPct: pooled.UsedPct, AccountsInRate: len(pooled.Windows), Accounts: []QuotaAccount{}}
	var start time.Time
	for _, win := range pooled.Windows {
		winStart := win.ResetsAt.Add(-quotaWindow)
		if start.IsZero() || winStart.Before(start) {
			start = winStart
		}
		w.Accounts = append(w.Accounts, QuotaAccount{
			AuthIndex: win.AuthIndex, UsedPct: win.UsedPct, WindowStart: utcSecond(winStart), ResetsAt: utcSecond(win.ResetsAt),
		})
	}
	for key, s := range a.series {
		if key.Provider == quotaProvider && (!fable || inFableScope(key.Model)) {
			w.CostUSDMicros += s.sumSince(start, currentID).CostUSDMicros
		}
	}
	if w.CostUSDMicros <= 0 {
		return nil
	}
	w.WindowStart = ptrTime(utcSecond(start))
	w.PctPerUSD = w.UsedPct / (float64(w.CostUSDMicros) / 1e6)
	return w
}

func ptrTime(t time.Time) *time.Time { return &t }
