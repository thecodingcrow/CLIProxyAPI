package claudequota

import "time"

// Pool is the weekly quota of the enabled accounts with data, per account, for pricing usage against it.
type Pool struct {
	// Accounts counts enabled accounts with data.
	Accounts   int
	ObservedAt time.Time
	// Stale is set when any pooled account holds a window read before its reset.
	Stale     bool
	WeeklyAll Pooled
	// Fable is the raw 7d_oi window, not the summary's max(Fable, weekly_all).
	Fable Pooled
}

// Pooled is one weekly window across accounts.
type Pooled struct {
	// UsedPct is the mean over the Pooled accounts reporting the window; an ended window counts 0.
	UsedPct float64
	Pooled  int
	// Windows lists the accounts whose window is still running, in account order.
	Windows []AccountWindow
}

// AccountWindow is one account's running window; it started ResetsAt minus 7 days.
type AccountWindow struct {
	AuthIndex string
	UsedPct   float64
	ResetsAt  time.Time
}

// Pool derives the pooled weekly windows at the snapshot's time.
func (s Snapshot) Pool() Pool {
	now := s.GeneratedAt
	var pool Pool
	var fableUsed []float64
	for _, account := range s.Accounts {
		if account.Disabled || account.HasQuota == nil {
			continue
		}
		pool.Accounts++
		pool.Stale = pool.Stale || account.Stale
		if account.ObservedAt != nil && account.ObservedAt.After(pool.ObservedAt) {
			pool.ObservedAt = *account.ObservedAt
		}
		if account.WeeklyAll != nil {
			pool.WeeklyAll.Pooled++
			pool.WeeklyAll.add(account.AuthIndex, account.WeeklyAll, now)
		}
		if fable, ok := account.WeeklyScoped[ScopeFable]; ok {
			pool.Fable.Pooled++
			fableUsed = append(fableUsed, fable.usedPercent(now))
			pool.Fable.add(account.AuthIndex, &fable, now)
		}
	}
	if s.Summary.WeeklyAllUsedPct != nil {
		pool.WeeklyAll.UsedPct = *s.Summary.WeeklyAllUsedPct
	}
	if used := mean(fableUsed); used != nil {
		pool.Fable.UsedPct = *used
	}
	return pool
}

// add lists limit when its reset is known and still ahead; otherwise its window start is unknown.
func (p *Pooled) add(authIndex string, limit *Limit, now time.Time) {
	if limit.ResetsAt == nil || limit.outlived(now) {
		return
	}
	p.Windows = append(p.Windows, AccountWindow{AuthIndex: authIndex, UsedPct: limit.usedPercent(now), ResetsAt: *limit.ResetsAt})
}
