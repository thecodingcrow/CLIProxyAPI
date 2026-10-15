package claudequota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeFetcherReadsUsageFromConfiguredBaseURL(t *testing.T) {
	body := loadFixture(t, "usage-weekly-exhausted.json")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer fake-token" || r.Header.Get("anthropic-beta") != claudeauth.UsageBeta {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	cfg, err := config.ParseConfigBytes([]byte("config-version: 8\nupstream:\n  claude:\n    usage-quota:\n      refresh-interval-seconds: 30\n      base-url: " + upstream.URL + "/\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if got := RefreshInterval(cfg); got != 30*time.Second {
		t.Fatalf("RefreshInterval = %v, want 30s from upstream.claude.usage-quota", got)
	}

	usage, err := ClaudeFetcher{Config: func() *config.Config { return cfg }}.Fetch(context.Background(), Credential{ID: "a", AccessToken: "fake-token"})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if usage.WeeklyAll == nil || usage.WeeklyAll.Percent != 100 || usage.Scoped["Fable"].Percent != 40 {
		t.Fatalf("usage = %+v, want recorded exhausted limits", usage)
	}
}

func TestClaudeFetcherSurfacesUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.Claude.UsageQuota.BaseURL = upstream.URL

	_, err := ClaudeFetcher{Config: func() *config.Config { return cfg }}.Fetch(context.Background(), Credential{ID: "a", AccessToken: "fake-token"})
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "fake-token") {
		t.Fatalf("Fetch() error = %v, want status 429 without token", err)
	}
}

func TestUsageEndpointDefaultsToAnthropic(t *testing.T) {
	if got := UsageEndpoint(&config.Config{}); got != "https://api.anthropic.com/api/oauth/usage" {
		t.Fatalf("UsageEndpoint = %q, want api.anthropic.com", got)
	}
}
