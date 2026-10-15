// Package claudequota derives per-credential Claude plan limits from the
// Anthropic-Ratelimit-Unified-* headers already observed on normal traffic and
// answers whether a credential still has quota for a model. It never calls upstream.
package claudequota

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ScopeFable names the model-scoped weekly window Anthropic reports as 7d_oi.
const ScopeFable = "Fable"

// exhaustedPercent is the usage at which Anthropic stops serving the window.
const exhaustedPercent = 100

const (
	unifiedHeaderPrefix = "Anthropic-Ratelimit-Unified-"
	windowSession       = "5h"
	windowWeeklyAll     = "7d"
	windowFable         = "7d_oi"
	statusRejected      = "rejected"
)

// Limit is one usage window of an account.
type Limit struct {
	Percent float64 `json:"percent"`
	// Status is the window's unified status (allowed, allowed_warning, rejected); empty when unreported.
	Status   string     `json:"status,omitempty"`
	ResetsAt *time.Time `json:"resets_at"`
	// ObservedAt is when the response carrying this window arrived.
	ObservedAt time.Time `json:"observed_at"`
}

// exhausted reports whether the window blocks requests at now. A window whose
// reset time has passed is treated as reset even before the next observation.
func (l *Limit) exhausted(now time.Time) bool {
	if l == nil || (l.Percent < exhaustedPercent && l.Status != statusRejected) {
		return false
	}
	return l.ResetsAt == nil || now.Before(*l.ResetsAt)
}

// outlived reports a reading older than its window's reset.
func (l *Limit) outlived(now time.Time) bool {
	return l != nil && l.ResetsAt != nil && !now.Before(*l.ResetsAt)
}

// Usage is the per-window view of one credential.
type Usage struct {
	Session   *Limit
	WeeklyAll *Limit
	// Scoped holds model-scoped weekly limits keyed by model display name.
	Scoped map[string]Limit
}

// Observation is one response's quota header snapshot.
type Observation struct {
	Signals    map[string]string
	ObservedAt time.Time
}

// UsageFrom takes each window from the newest observation reporting it, so a
// response that skipped a window does not erase the last reading of it. Nil
// when no observation reports any window.
func UsageFrom(observations []Observation) *Usage {
	newest := append([]Observation(nil), observations...)
	sort.SliceStable(newest, func(i, j int) bool { return newest[i].ObservedAt.After(newest[j].ObservedAt) })
	usage := &Usage{Scoped: map[string]Limit{}}
	for _, obs := range newest {
		if usage.Session == nil {
			usage.Session = parseWindow(obs, windowSession)
		}
		if usage.WeeklyAll == nil {
			usage.WeeklyAll = parseWindow(obs, windowWeeklyAll)
		}
		if _, ok := usage.Scoped[ScopeFable]; !ok {
			if limit := parseWindow(obs, windowFable); limit != nil {
				usage.Scoped[ScopeFable] = *limit
			}
		}
	}
	if usage.Session == nil && usage.WeeklyAll == nil && len(usage.Scoped) == 0 {
		return nil
	}
	return usage
}

// parseWindow reads one window; nil unless it reports a utilization or a rejection.
func parseWindow(obs Observation, window string) *Limit {
	prefix := unifiedHeaderPrefix + window + "-"
	status := strings.ToLower(strings.TrimSpace(obs.Signals[prefix+"Status"]))
	utilization, errParse := strconv.ParseFloat(strings.TrimSpace(obs.Signals[prefix+"Utilization"]), 64)
	hasUtilization := errParse == nil && utilization >= 0 && !math.IsInf(utilization, 0)
	if !hasUtilization && status != statusRejected {
		return nil
	}
	limit := &Limit{Percent: exhaustedPercent, Status: status, ObservedAt: obs.ObservedAt}
	if hasUtilization {
		// Utilization is a fraction; rounding hides float noise (0.55*100).
		limit.Percent = math.Round(utilization*10000) / 100
	}
	if seconds, errReset := strconv.ParseFloat(strings.TrimSpace(obs.Signals[prefix+"Reset"]), 64); errReset == nil && seconds > 0 && !math.IsInf(seconds, 0) {
		reset := time.Unix(int64(seconds), 0).UTC()
		limit.ResetsAt = &reset
	}
	return limit
}

// observedAt is the newest observation among the windows.
func (u *Usage) observedAt() time.Time {
	var newest time.Time
	for _, limit := range u.limits() {
		if limit.ObservedAt.After(newest) {
			newest = limit.ObservedAt
		}
	}
	return newest
}

// stale reports a window read before its own reset; that window counts as reset.
func (u *Usage) stale(now time.Time) bool {
	for _, limit := range u.limits() {
		if limit.outlived(now) {
			return true
		}
	}
	return false
}

func (u *Usage) limits() []*Limit {
	limits := make([]*Limit, 0, 2+len(u.Scoped))
	if u.Session != nil {
		limits = append(limits, u.Session)
	}
	if u.WeeklyAll != nil {
		limits = append(limits, u.WeeklyAll)
	}
	for _, limit := range u.Scoped {
		limits = append(limits, &limit)
	}
	return limits
}

// HasQuota reports whether the account can serve model at now: session and
// weekly_all not exhausted and, when a scoped limit matches model, that one too.
// An empty model checks only the unscoped windows.
func (u *Usage) HasQuota(model string, now time.Time) bool {
	if u == nil {
		return false
	}
	blocked, _ := u.blockedUntil(model, now)
	return !blocked
}

// blockedUntil reports whether the windows HasQuota checks block model at now
// and when the last of them resets; zero when one reports no reset time.
func (u *Usage) blockedUntil(model string, now time.Time) (blocked bool, until time.Time) {
	unknownReset := false
	check := func(limit *Limit) {
		if !limit.exhausted(now) {
			return
		}
		blocked = true
		switch {
		case limit.ResetsAt == nil:
			unknownReset = true
		case limit.ResetsAt.After(until):
			until = *limit.ResetsAt
		}
	}
	check(u.Session)
	check(u.WeeklyAll)
	for name, limit := range u.Scoped {
		if ScopeMatchesModel(name, model) {
			check(&limit)
		}
	}
	if unknownReset {
		return blocked, time.Time{}
	}
	return blocked, until
}

// ScopeMatchesModel reports whether a scoped limit's display name names model:
// case-insensitive, either equal or present as whole '-'-separated tokens
// ("Nimbus Max" matches "claude-nimbus-max-2"; "Opus 4" does not match "claude-opus-45").
func ScopeMatchesModel(displayName, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	name := strings.ToLower(strings.TrimSpace(displayName))
	if model == "" || name == "" {
		return false
	}
	if model == name {
		return true
	}
	token := strings.Join(strings.Fields(name), "-")
	for offset := 0; offset <= len(model)-len(token); {
		at := strings.Index(model[offset:], token)
		if at < 0 {
			return false
		}
		start, end := offset+at, offset+at+len(token)
		if (start == 0 || model[start-1] == '-') && (end == len(model) || model[end] == '-') {
			return true
		}
		offset = start + 1
	}
	return false
}
