package api

import (
	"context"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// newClaudeQuotaStore wires the plan-limit cache to the auth manager and the
// live config; polling stays off until upstream.claude.usage-quota.enabled.
func (s *Server) newClaudeQuotaStore(manager *auth.Manager, opts claudequota.Options) *claudequota.Store {
	return claudequota.NewStore(
		func() []claudequota.Credential { return claudeQuotaCredentials(manager.List()) },
		claudequota.ClaudeFetcher{Config: s.getConfig},
		func() claudequota.Settings { return claudequota.SettingsFromConfig(s.getConfig()) },
		opts,
	)
}

// startClaudeQuota runs the refresher until stopClaudeQuota.
func (s *Server) startClaudeQuota() {
	if s.claudeQuota == nil {
		return
	}
	s.claudeQuotaMu.Lock()
	defer s.claudeQuotaMu.Unlock()
	if s.claudeQuotaCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.claudeQuotaCancel = cancel
	go s.claudeQuota.Run(ctx)
}

func (s *Server) stopClaudeQuota() {
	s.claudeQuotaMu.Lock()
	defer s.claudeQuotaMu.Unlock()
	if s.claudeQuotaCancel != nil {
		s.claudeQuotaCancel()
		s.claudeQuotaCancel = nil
	}
}

// claudeQuotaCredentials selects Claude OAuth credentials (API-key entries have
// no plan limits) and copies their routing-relevant state.
func claudeQuotaCredentials(auths []*auth.Auth) []claudequota.Credential {
	creds := make([]claudequota.Credential, 0, len(auths))
	for _, a := range auths {
		if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "claude") {
			continue
		}
		if a.Attributes != nil && strings.TrimSpace(a.Attributes["api_key"]) != "" {
			continue
		}
		token := claudeauth.ReadMetadataString(&a.Metadata, "access_token")
		if token == "" && claudeauth.ReadMetadataString(&a.Metadata, "refresh_token") == "" {
			continue
		}
		name := strings.TrimSpace(a.FileName)
		if name == "" {
			name = a.ID
		}
		cred := claudequota.Credential{
			ID:          a.ID,
			Index:       a.EnsureIndex(),
			Name:        name,
			Label:       a.Label,
			Email:       strings.TrimSpace(claudeauth.ReadMetadataString(&a.Metadata, "email")),
			Disabled:    a.Disabled || a.Status == auth.StatusDisabled,
			AccessToken: token,
			ProxyURL:    a.ProxyURL,
		}
		if a.Quota.Exceeded {
			cred.CooldownUntil = a.Quota.NextRecoverAt
		}
		for model, state := range a.ModelStates {
			if state == nil || !state.Quota.Exceeded {
				continue
			}
			if cred.ModelCooldowns == nil {
				cred.ModelCooldowns = map[string]time.Time{}
			}
			cred.ModelCooldowns[model] = state.Quota.NextRecoverAt
		}
		creds = append(creds, cred)
	}
	return creds
}
