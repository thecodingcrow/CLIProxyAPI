package claudequota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixtureNow sits between the recorded responses' as_of and their earliest reset.
var fixtureNow = time.Date(2026, 10, 15, 11, 7, 50, 0, time.UTC)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

func parseFixture(t *testing.T, name string) *Usage {
	t.Helper()
	usage, err := ParseUsage(loadFixture(t, name))
	if err != nil {
		t.Fatalf("ParseUsage(%s) error = %v", name, err)
	}
	return usage
}

func TestParseUsageReadsRecordedLimits(t *testing.T) {
	healthy := parseFixture(t, "usage-healthy.json")
	if healthy.Session == nil || healthy.Session.Percent != 31 || healthy.Session.Severity != "normal" || !healthy.Session.IsActive {
		t.Fatalf("healthy session = %+v, want 31%% normal active", healthy.Session)
	}
	wantSessionReset := time.Date(2026, 10, 15, 14, 19, 59, 687096000, time.UTC)
	if healthy.Session.ResetsAt == nil || !healthy.Session.ResetsAt.Equal(wantSessionReset) {
		t.Fatalf("healthy session resets_at = %v, want %v", healthy.Session.ResetsAt, wantSessionReset)
	}
	if healthy.WeeklyAll == nil || healthy.WeeklyAll.Percent != 9 {
		t.Fatalf("healthy weekly_all = %+v, want 9%%", healthy.WeeklyAll)
	}
	if fable, ok := healthy.Scoped["Fable"]; !ok || fable.Percent != 7 {
		t.Fatalf("healthy scoped = %+v, want Fable 7%%", healthy.Scoped)
	}

	exhausted := parseFixture(t, "usage-weekly-exhausted.json")
	if exhausted.Session == nil || exhausted.Session.ResetsAt != nil || exhausted.Session.Percent != 0 {
		t.Fatalf("exhausted session = %+v, want 0%% with null resets_at", exhausted.Session)
	}
	if exhausted.WeeklyAll == nil || exhausted.WeeklyAll.Percent != 100 || exhausted.WeeklyAll.Severity != "critical" {
		t.Fatalf("exhausted weekly_all = %+v, want 100%% critical", exhausted.WeeklyAll)
	}
	if fable := exhausted.Scoped["Fable"]; fable.Percent != 40 {
		t.Fatalf("exhausted Fable = %+v, want 40%%", fable)
	}
}

func TestUsageHasQuotaRequiresSessionWeeklyAndScopedHeadroom(t *testing.T) {
	healthy := parseFixture(t, "usage-healthy.json")
	exhausted := parseFixture(t, "usage-weekly-exhausted.json")
	cases := []struct {
		name  string
		usage *Usage
		model string
		want  bool
	}{
		{"healthy serves Fable", healthy, "claude-fable-5", true},
		{"healthy serves unscoped model", healthy, "claude-sonnet-4-5", true},
		{"weekly_all 100 blocks Fable despite scoped 40", exhausted, "claude-fable-5", false},
		{"weekly_all 100 blocks unscoped model", exhausted, "claude-sonnet-4-5", false},
		{"weekly_all 100 blocks model-agnostic check", exhausted, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.HasQuota(tc.model, fixtureNow); got != tc.want {
				t.Fatalf("HasQuota(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestUsageHasQuotaScopedLimitOnlyBlocksMatchingModel(t *testing.T) {
	usage, err := ParseUsage([]byte(`{"limits":[
		{"kind":"session","percent":10,"resets_at":null,"scope":null},
		{"kind":"weekly_all","percent":50,"resets_at":"2026-10-20T10:00:00+00:00","scope":null},
		{"kind":"weekly_scoped","percent":100,"resets_at":"2026-10-20T10:00:00+00:00","scope":{"model":{"id":null,"display_name":"Nimbus Max"},"surface":null}}
	]}`))
	if err != nil {
		t.Fatalf("ParseUsage() error = %v", err)
	}
	if usage.HasQuota("claude-nimbus-max-2", fixtureNow) {
		t.Fatal("HasQuota(nimbus-max) = true, want false: its scoped weekly is 100")
	}
	if !usage.HasQuota("claude-fable-5", fixtureNow) {
		t.Fatal("HasQuota(fable) = false, want true: only Nimbus Max is exhausted")
	}
}

func TestUsageHasQuotaIgnoresExhaustedLimitPastItsReset(t *testing.T) {
	exhausted := parseFixture(t, "usage-weekly-exhausted.json")
	afterReset := time.Date(2026, 10, 20, 10, 0, 1, 0, time.UTC)
	if !exhausted.HasQuota("claude-fable-5", afterReset) {
		t.Fatal("HasQuota after weekly reset = false, want true")
	}
}

func TestParseUsageToleratesNullAndMalformedFields(t *testing.T) {
	bodies := []string{
		`{}`,
		`{"limits":null}`,
		`{"limits":[null,{"kind":null},{"kind":"session","percent":null,"resets_at":"not-a-time"}]}`,
		`{"limits":[{"kind":"weekly_scoped","percent":5,"scope":null},{"kind":"weekly_scoped","percent":5,"scope":{"model":null}}]}`,
		`{"limits":[{"kind":"session","percent":"12"}],"five_hour":{"utilization":"x"}}`,
	}
	for _, body := range bodies {
		usage, err := ParseUsage([]byte(body))
		if err != nil {
			t.Fatalf("ParseUsage(%s) error = %v", body, err)
		}
		if !usage.HasQuota("claude-fable-5", fixtureNow) {
			t.Fatalf("ParseUsage(%s).HasQuota = false, want true with no blocking limit", body)
		}
	}
	if _, err := ParseUsage([]byte(`not json`)); err == nil {
		t.Fatal("ParseUsage(not json) error = nil, want error")
	}
}
