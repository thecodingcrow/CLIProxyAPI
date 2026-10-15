package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelusage"
)

func TestManagementV8ModelUsageRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\nremote-management: {secret-key: test-password}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := management.NewHandler(cfg, path, nil)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()

	// Provider unique to this test: the default aggregator is process-wide.
	const provider = "route-test-provider"
	modelusage.Default().Add(modelusage.Key{Provider: provider, Model: "m", Subagent: true},
		modelusage.Counters{Requests: 1, InputTokens: 4, OutputTokens: 6, TotalTokens: 10})
	modelusage.Default().Add(modelusage.Key{Provider: provider, Model: "m"},
		modelusage.Counters{Requests: 2, Failed: 1, TotalTokens: 3})

	get := func(authorized bool, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/models"+query, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(false, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := get(true, "?range=week"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("invalid range: status=%d body=%s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct{ query, wantRange string }{
		{"", "window"}, {"?range=window", "window"}, {"?range=today", "today"}, {"?range=7d", "7d"}, {"?range=total", "total"},
	} {
		t.Run("range "+tc.wantRange+tc.query, func(t *testing.T) {
			checkModelUsageResponse(t, get(true, tc.query), provider, tc.wantRange)
		})
	}
}

func checkModelUsageResponse(t *testing.T, rec *httptest.ResponseRecorder, provider, wantRange string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Range         string `json:"range"`
		Since         string `json:"since"`
		Timezone      string `json:"timezone"`
		BucketSeconds int64  `json:"bucket_seconds"`
		WindowMinutes int64  `json:"window_minutes"`
		GeneratedAt   string `json:"generated_at"`
		Models        []struct {
			Provider     string `json:"provider"`
			Model        string `json:"model"`
			Subagent     bool   `json:"subagent"`
			Requests     int64  `json:"requests"`
			Failed       int64  `json:"failed"`
			OutputTokens int64  `json:"output_tokens"`
			TotalTokens  int64  `json:"total_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body.BucketSeconds != 600 || body.GeneratedAt == "" || body.Range != wantRange || body.Timezone == "" {
		t.Fatalf("envelope: %s", rec.Body.String())
	}
	since, errSince := time.Parse(time.RFC3339, body.Since)
	if errSince != nil {
		t.Fatalf("since: %v body=%s", errSince, rec.Body.String())
	}
	if wantMinutes := map[string]int64{"window": 200, "7d": 7 * 24 * 60}[wantRange]; wantMinutes != 0 && body.WindowMinutes != wantMinutes {
		t.Fatalf("window_minutes %d, want %d", body.WindowMinutes, wantMinutes)
	}
	// One minute of slack: the handler's clock read precedes this one.
	if got := int64(time.Since(since).Round(time.Minute) / time.Minute); got < body.WindowMinutes || got > body.WindowMinutes+1 {
		t.Fatalf("window_minutes %d does not span since %s", body.WindowMinutes, body.Since)
	}
	var rows []int
	for i, m := range body.Models {
		if m.Provider == provider {
			rows = append(rows, i)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("want main + subagent rows, got %s", rec.Body.String())
	}
	first, second := body.Models[rows[0]], body.Models[rows[1]]
	if !first.Subagent || first.TotalTokens != 10 || first.OutputTokens != 6 {
		t.Fatalf("subagent row (highest total first): %+v", first)
	}
	if second.Subagent || second.Requests != 2 || second.Failed != 1 {
		t.Fatalf("main row: %+v", second)
	}
}
