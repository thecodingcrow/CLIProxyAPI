package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

	get := func(authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/models", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if authorized {
			req.Header.Set("Authorization", "Bearer test-password")
		}
		rec := httptest.NewRecorder()
		s.engine.ServeHTTP(rec, req)
		return rec
	}

	if rec := get(false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := get(true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
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
	if body.BucketSeconds != 600 || body.WindowMinutes != 200 || body.GeneratedAt == "" {
		t.Fatalf("envelope: %s", rec.Body.String())
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
