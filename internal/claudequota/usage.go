// Package claudequota caches the plan limits Anthropic reports per Claude OAuth
// credential (GET /api/oauth/usage) and answers whether a credential still has
// quota for a model. Reads are memory-only; a background refresher owns every
// upstream call.
package claudequota

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Limit kinds reported by the usage endpoint.
const (
	KindSession      = "session"
	KindWeeklyAll    = "weekly_all"
	KindWeeklyScoped = "weekly_scoped"
)

// exhaustedPercent is the usage at which Anthropic stops serving the window.
const exhaustedPercent = 100

// Limit is one usage window of an account.
type Limit struct {
	Percent  float64    `json:"percent"`
	Severity string     `json:"severity,omitempty"`
	ResetsAt *time.Time `json:"resets_at"`
	IsActive bool       `json:"is_active"`
}

// exhausted reports whether the window blocks requests at now. A window whose
// reset time has passed is treated as reset even before the next fetch.
func (l *Limit) exhausted(now time.Time) bool {
	if l == nil || l.Percent < exhaustedPercent {
		return false
	}
	return l.ResetsAt == nil || now.Before(*l.ResetsAt)
}

// Usage is the decoded subset of one usage response.
type Usage struct {
	Session   *Limit
	WeeklyAll *Limit
	// Scoped holds model-scoped weekly limits keyed by scope.model.display_name.
	Scoped map[string]Limit
}

type rawLimit struct {
	Kind     string          `json:"kind"`
	Percent  json.RawMessage `json:"percent"`
	Severity string          `json:"severity"`
	ResetsAt json.RawMessage `json:"resets_at"`
	IsActive bool            `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// ParseUsage decodes the limits[] array of a usage response. Unknown keys,
// unknown kinds and individually malformed limits are skipped; only a body
// that is not a JSON object fails.
func ParseUsage(body []byte) (*Usage, error) {
	var envelope struct {
		Limits []json.RawMessage `json:"limits"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("parse Claude usage response: %w", err)
	}
	usage := &Usage{Scoped: map[string]Limit{}}
	for _, item := range envelope.Limits {
		var raw rawLimit
		if err := json.Unmarshal(item, &raw); err != nil {
			continue
		}
		var percent float64
		if err := json.Unmarshal(raw.Percent, &percent); err != nil {
			continue
		}
		limit := Limit{Percent: percent, Severity: raw.Severity, IsActive: raw.IsActive, ResetsAt: parseResetsAt(raw.ResetsAt)}
		switch raw.Kind {
		case KindSession:
			usage.Session = maxLimit(usage.Session, limit)
		case KindWeeklyAll:
			usage.WeeklyAll = maxLimit(usage.WeeklyAll, limit)
		case KindWeeklyScoped:
			if raw.Scope == nil || raw.Scope.Model == nil {
				continue
			}
			name := strings.TrimSpace(raw.Scope.Model.DisplayName)
			if name == "" {
				continue
			}
			if existing, ok := usage.Scoped[name]; !ok || limit.Percent > existing.Percent {
				usage.Scoped[name] = limit
			}
		}
	}
	return usage, nil
}

func maxLimit(current *Limit, next Limit) *Limit {
	if current != nil && current.Percent >= next.Percent {
		return current
	}
	return &next
}

func parseResetsAt(raw json.RawMessage) *time.Time {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

// HasQuota reports whether the account can serve model at now: session and
// weekly_all below 100% and, when a scoped limit matches model, that one too.
// An empty model checks only the unscoped windows.
func (u *Usage) HasQuota(model string, now time.Time) bool {
	if u == nil {
		return false
	}
	if u.Session.exhausted(now) || u.WeeklyAll.exhausted(now) {
		return false
	}
	for name, limit := range u.Scoped {
		if ScopeMatchesModel(name, model) && limit.exhausted(now) {
			return false
		}
	}
	return true
}

// ScopeMatchesModel reports whether a scoped limit's display name names model:
// case-insensitive, either equal or contained as a hyphenated token
// ("Nimbus Max" matches "claude-nimbus-max-2").
func ScopeMatchesModel(displayName, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	name := strings.ToLower(strings.TrimSpace(displayName))
	if model == "" || name == "" {
		return false
	}
	if model == name {
		return true
	}
	return strings.Contains(model, strings.Join(strings.Fields(name), "-"))
}
