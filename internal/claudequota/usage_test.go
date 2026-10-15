package claudequota

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// observedNow is when the recorded header sets below arrived.
var observedNow = time.Unix(1787279282, 0).UTC()

var (
	sessionReset = time.Unix(1787296800, 0).UTC()
	weeklyReset  = time.Unix(1787695200, 0).UTC()
	fableReset   = time.Unix(1787600000, 0).UTC()
)

// observe stores headers the way the auth manager does for a Claude response.
func observe(t *testing.T, headers http.Header, at time.Time) Observation {
	t.Helper()
	var quota auth.QuotaState
	if !quota.ObserveResponseHeadersForProvider("claude", headers, at) {
		t.Fatalf("headers %v carried no quota signal", headers)
	}
	return Observation{Signals: quota.Signals, ObservedAt: quota.ObservedAt}
}

func unix(at time.Time) []string { return []string{strconv.FormatInt(at.Unix(), 10)} }

// recordedAllowed is a measured response on an unscoped model: 5h and 7d only.
func recordedAllowed() http.Header {
	return http.Header{
		"Anthropic-Ratelimit-Unified-5h-Status":               {"allowed"},
		"Anthropic-Ratelimit-Unified-5h-Utilization":          {"0.0"},
		"Anthropic-Ratelimit-Unified-5h-Reset":                unix(sessionReset),
		"Anthropic-Ratelimit-Unified-7d-Status":               {"allowed"},
		"Anthropic-Ratelimit-Unified-7d-Utilization":          {"0.53"},
		"Anthropic-Ratelimit-Unified-7d-Reset":                unix(weeklyReset),
		"Anthropic-Ratelimit-Unified-Overage-Disabled-Reason": {"member_zero_credit_limit"},
		"Anthropic-Ratelimit-Unified-Overage-Status":          {"rejected"},
		"Anthropic-Ratelimit-Unified-Representative-Claim":    {"five_hour"},
		"Anthropic-Ratelimit-Unified-Reset":                   unix(sessionReset),
		"Anthropic-Ratelimit-Unified-Status":                  {"allowed"},
	}
}

// recordedFableRejected is a Fable-window rejection: shared windows allowed, 7d_oi over.
func recordedFableRejected() http.Header {
	return http.Header{
		"anthropic-ratelimit-unified-status":                  {"rejected"},
		"anthropic-ratelimit-unified-representative-claim":    {"seven_day_overage_included"},
		"anthropic-ratelimit-unified-7d-status":               {"allowed"},
		"anthropic-ratelimit-unified-7d-utilization":          {"0.69"},
		"anthropic-ratelimit-unified-7d-reset":                unix(weeklyReset),
		"anthropic-ratelimit-unified-5h-utilization":          {"0.00"},
		"anthropic-ratelimit-unified-7d_oi-status":            {"rejected"},
		"anthropic-ratelimit-unified-7d_oi-utilization":       {"1.02"},
		"anthropic-ratelimit-unified-7d_oi-reset":             unix(fableReset),
		"anthropic-ratelimit-unified-overage-status":          {"rejected"},
		"anthropic-ratelimit-unified-overage-disabled-reason": {"org_spend_cap_reached"},
	}
}

func percentOf(limit *Limit) float64 {
	if limit == nil {
		return -1
	}
	return limit.Percent
}

func TestUsageFromHeaders(t *testing.T) {
	cases := []struct {
		name                   string
		headers                http.Header
		now                    time.Time
		session, weekly, fable float64 // -1: window absent
		fableModel, otherModel bool
	}{
		{"recorded allowed", recordedAllowed(), observedNow, 0, 53, -1, true, true},
		{"recorded Fable rejection", recordedFableRejected(), observedNow, 0, 69, 102, false, true},
		{"rejected 7d without utilization", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Status": {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Reset":  unix(weeklyReset),
		}, observedNow, -1, 100, -1, false, false},
		{"rejected 7d_oi below 100 still blocks Fable", http.Header{
			"Anthropic-Ratelimit-Unified-7d_oi-Status":      {"rejected"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"0.98"},
		}, observedNow, -1, -1, 98, false, true},
		{"exhausted 7d past its reset counts as reset", http.Header{
			"Anthropic-Ratelimit-Unified-7d-Status":      {"rejected"},
			"Anthropic-Ratelimit-Unified-7d-Utilization": {"1.0"},
			"Anthropic-Ratelimit-Unified-7d-Reset":       unix(weeklyReset),
		}, weeklyReset.Add(time.Second), -1, 100, -1, true, true},
		{"allowed_warning near the limit", http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status":      {"allowed_warning"},
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.91"},
		}, observedNow, 91, -1, -1, true, true},
		{"status without utilization claims nothing", http.Header{
			"Anthropic-Ratelimit-Unified-5h-Status":         {"allowed"},
			"Anthropic-Ratelimit-Unified-7d-Utilization":    {"not-a-number"},
			"Anthropic-Ratelimit-Unified-7d_oi-Utilization": {"NaN"},
			"Anthropic-Ratelimit-Unified-Status":            {"allowed"},
		}, observedNow, -1, -1, -1, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := UsageFrom([]Observation{observe(t, tc.headers, observedNow)})
			if tc.session < 0 && tc.weekly < 0 && tc.fable < 0 {
				if usage != nil {
					t.Fatalf("usage = %+v, want nil without any window", usage)
				}
				return
			}
			var fable *Limit
			if limit, ok := usage.Scoped[ScopeFable]; ok {
				fable = &limit
			}
			if percentOf(usage.Session) != tc.session || percentOf(usage.WeeklyAll) != tc.weekly || percentOf(fable) != tc.fable {
				t.Fatalf("percent session/weekly/fable = %v/%v/%v, want %v/%v/%v", percentOf(usage.Session), percentOf(usage.WeeklyAll), percentOf(fable), tc.session, tc.weekly, tc.fable)
			}
			if got := usage.HasQuota("claude-fable-5", tc.now); got != tc.fableModel {
				t.Errorf("HasQuota(fable) = %v, want %v", got, tc.fableModel)
			}
			if got := usage.HasQuota("claude-sonnet-4-5", tc.now); got != tc.otherModel {
				t.Errorf("HasQuota(sonnet) = %v, want %v", got, tc.otherModel)
			}
		})
	}
}

func TestUsageFromReadsResetsAsUnixSeconds(t *testing.T) {
	usage := UsageFrom([]Observation{observe(t, recordedFableRejected(), observedNow)})
	if usage.WeeklyAll.ResetsAt == nil || !usage.WeeklyAll.ResetsAt.Equal(weeklyReset) {
		t.Fatalf("weekly resets_at = %v, want %v", usage.WeeklyAll.ResetsAt, weeklyReset)
	}
	if usage.Session.ResetsAt != nil {
		t.Fatalf("session resets_at = %v, want nil when unreported", usage.Session.ResetsAt)
	}
	if blocked, until := usage.blockedUntil("claude-fable-5", observedNow); !blocked || !until.Equal(fableReset) {
		t.Fatalf("blockedUntil(fable) = %v/%v, want blocked until %v", blocked, until, fableReset)
	}
}

func TestUsageFromTakesEachWindowFromItsNewestObservation(t *testing.T) {
	older := observe(t, recordedFableRejected(), observedNow)
	newer := observe(t, recordedAllowed(), observedNow.Add(time.Minute))
	usage := UsageFrom([]Observation{newer, older})
	fable, ok := usage.Scoped[ScopeFable]
	if !ok || fable.Percent != 102 || !fable.ObservedAt.Equal(observedNow) {
		t.Fatalf("Fable = %+v (present %v), want 102 kept from the older observation", fable, ok)
	}
	if usage.WeeklyAll.Percent != 53 || usage.Session.Status != "allowed" {
		t.Fatalf("weekly/session = %+v / %+v, want the newer 53 and allowed", usage.WeeklyAll, usage.Session)
	}
	if got := usage.observedAt(); !got.Equal(observedNow.Add(time.Minute)) {
		t.Fatalf("observedAt = %v, want the newest observation", got)
	}
}

func TestUsageStaleOnceAWindowOutlivesItsReset(t *testing.T) {
	usage := UsageFrom([]Observation{observe(t, recordedAllowed(), observedNow)})
	if usage.stale(sessionReset.Add(-time.Second)) {
		t.Fatal("stale before any reset")
	}
	if !usage.stale(sessionReset) {
		t.Fatal("not stale once the 5h window reset")
	}
}

func TestScopeMatchesModelOnTokenBoundaries(t *testing.T) {
	cases := []struct {
		scope, model string
		want         bool
	}{
		{"Fable", "claude-fable-5-1", true},
		{"Fable", "claude-fable", true},
		{"fable", "Claude-Fable-5", true},
		{"Nimbus Max", "claude-nimbus-max-2", true},
		{"Opus 4", "claude-opus-4-1", true},
		{"Opus 4", "claude-opus-45", false},
		{"Fable", "claude-fables-5", false},
		{"Fable", "claude-superfable-5", false},
		{"Fable", "", false},
	}
	for _, tc := range cases {
		if got := ScopeMatchesModel(tc.scope, tc.model); got != tc.want {
			t.Errorf("ScopeMatchesModel(%q, %q) = %v, want %v", tc.scope, tc.model, got, tc.want)
		}
	}
}
