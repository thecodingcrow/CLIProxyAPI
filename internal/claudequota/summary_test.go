package claudequota

import (
	"testing"
	"time"
)

func TestSummarizeScopedCountsAccountWithoutScopedReadingAtItsWeeklyUse(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	reset := now.Add(24 * time.Hour)
	limit := func(percent float64) *Limit { return &Limit{Percent: percent, ResetsAt: &reset, endsBy: reset} }
	accounts := []Account{
		// out of weekly quota, no Fable reading: cannot serve Fable
		{HasQuota: &HasQuota{}, WeeklyAll: limit(100)},
		{HasQuota: &HasQuota{Any: true}, WeeklyAll: limit(29), WeeklyScoped: map[string]Limit{ScopeFable: *limit(43)}},
	}

	fable := summarize(accounts, now).Scoped[ScopeFable]

	if fable.UsedPct != 71.5 || fable.RemainingPct != 28.5 || fable.Accounts != 2 {
		t.Fatalf("Fable = %+v, want used 71.5 (mean of 100 and 43), remaining 28.5, 2 accounts", fable)
	}
}
