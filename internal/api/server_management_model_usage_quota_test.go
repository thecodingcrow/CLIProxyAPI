package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelusage"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestModelUsagePricesClaudeRowsAgainstPooledQuota(t *testing.T) {
	guardExternalDials(t)
	q := newQuotaTestServer(t, true)
	q.respond("healthy", "claude-sonnet-5-5", unifiedHeaders(q.clock.Now(), "0.10", "0.40", "0.20", "allowed"))
	_, quota, _ := q.get(t, "test-password")
	index := snapshotAccount(t, quota, "healthy").AuthIndex
	getModels := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/models?range=7d", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer test-password")
		rec := httptest.NewRecorder()
		q.server.engine.ServeHTTP(rec, req)
		return rec
	}
	// The window began before tracking: the first read sets the 40% baseline, the rate needs a 5-point change.
	getModels()
	// Model unique to this test: the default aggregator is process-wide.
	const model = "claude-sonnet-5-5-20990101"
	modelusage.Default().HandleUsage(context.Background(), coreusage.Record{
		Provider: "claude", Model: model, AuthIndex: index,
		Detail: coreusage.Detail{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100},
	})
	q.respond("healthy", "claude-sonnet-5-5", unifiedHeaders(q.clock.Now(), "0.10", "0.46", "0.20", "allowed"))
	rec := getModels()
	var body struct {
		Models []struct {
			Model          string   `json:"model"`
			CostUSDMicros  int64    `json:"cost_usd_micros"`
			QuotaWeeklyPct *float64 `json:"quota_weekly_pct"`
		} `json:"models"`
		Quota *struct {
			Provider  string `json:"provider"`
			Accounts  int    `json:"accounts"`
			WeeklyAll *struct {
				UsedPct  float64 `json:"used_pct"`
				Accounts []struct {
					AuthIndex     string `json:"auth_index"`
					CostUSDMicros int64  `json:"cost_usd_micros"`
				} `json:"accounts"`
			} `json:"weekly_all"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d decode %v: %s", rec.Code, err, rec.Body.String())
	}
	if body.Quota == nil || body.Quota.Provider != "claude" || body.Quota.Accounts != 1 || body.Quota.WeeklyAll == nil || body.Quota.WeeklyAll.UsedPct != 46 {
		t.Fatalf("quota block: %s", rec.Body.String())
	}
	found, rowFound := false, false
	for _, a := range body.Quota.WeeklyAll.Accounts {
		found = found || (a.AuthIndex == index && a.CostUSDMicros >= 3000)
	}
	for _, row := range body.Models {
		rowFound = rowFound || row.Model == model
		if row.Model == model && (row.CostUSDMicros != 3000 || row.QuotaWeeklyPct == nil) {
			t.Fatalf("row %+v: want 3000 µUSD with quota_weekly_pct", row)
		}
	}
	if !found || !rowFound {
		t.Fatalf("account %s missing from weekly_all: %s", index, rec.Body.String())
	}
}
