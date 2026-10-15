package claudequota

import (
	"context"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const usagePath = "/api/oauth/usage"

// ClaudeFetcher calls the OAuth usage endpoint through the Claude control-plane
// uTLS client, honouring the credential's proxy. It sends the credential's
// current access token; token refresh stays with the auth manager.
type ClaudeFetcher struct {
	// Config returns the live configuration, read per fetch for hot reload.
	Config func() *config.Config
}

// Fetch implements Fetcher.
func (f ClaudeFetcher) Fetch(ctx context.Context, cred Credential) (*Usage, error) {
	var cfg *config.Config
	if f.Config != nil {
		cfg = f.Config()
	}
	body, err := claudeauth.NewClaudeAuthWithProxyURL(cfg, cred.ProxyURL).FetchOAuthUsage(ctx, UsageEndpoint(cfg), cred.AccessToken)
	if err != nil {
		return nil, err
	}
	return ParseUsage(body)
}

// UsageEndpoint resolves the usage URL, honouring upstream.claude.usage-quota.base-url.
func UsageEndpoint(cfg *config.Config) string {
	if cfg == nil {
		return claudeauth.UsageURL
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Claude.UsageQuota.BaseURL), "/")
	if base == "" {
		return claudeauth.UsageURL
	}
	return base + usagePath
}

// RefreshInterval converts the configured seconds for NewStore.
func RefreshInterval(cfg *config.Config) time.Duration {
	if cfg == nil {
		return 0
	}
	return time.Duration(cfg.Claude.UsageQuota.RefreshIntervalSeconds) * time.Second
}
