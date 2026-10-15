package claudequota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// Fetch implements Fetcher. A 401/403 wraps ErrCredentialRejected.
func (f ClaudeFetcher) Fetch(ctx context.Context, cred Credential) (*Usage, error) {
	var cfg *config.Config
	if f.Config != nil {
		cfg = f.Config()
	}
	body, err := claudeauth.NewClaudeAuthWithProxyURL(cfg, cred.ProxyURL).FetchOAuthUsage(ctx, UsageEndpoint(cfg), cred.AccessToken)
	if err != nil {
		var statusErr *claudeauth.ControlPlaneStatusError
		if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w: %v", ErrCredentialRejected, err)
		}
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

// SettingsFromConfig maps upstream.claude.usage-quota onto store settings.
func SettingsFromConfig(cfg *config.Config) Settings {
	if cfg == nil {
		return Settings{}
	}
	quota := cfg.Claude.UsageQuota
	return Settings{Enabled: quota.Enabled, Interval: time.Duration(quota.RefreshIntervalMinutes) * time.Minute}
}
