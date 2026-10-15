package api

import (
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// attachClaudeQuota derives plan limits from the quota headers the auth
// manager stores per credential and hands them to the management endpoint
// and to quota-first routing.
func (s *Server) attachClaudeQuota(manager *auth.Manager, now func() time.Time) {
	s.claudeQuota = claudequota.NewStore(
		func() []claudequota.Credential {
			auths := manager.List()
			creds := make([]claudequota.Credential, 0, len(auths))
			for _, a := range auths {
				if cred, ok := claudeQuotaCredential(a); ok {
					creds = append(creds, cred)
				}
			}
			return creds
		},
		func(id string) (claudequota.Credential, bool) {
			a, ok := manager.GetByID(id)
			if !ok {
				return claudequota.Credential{}, false
			}
			return claudeQuotaCredential(a)
		},
		now,
	)
	s.mgmt.SetClaudeQuota(s.claudeQuota)
	manager.SetQuotaProvider(claudeQuotaRouting{reader: s.claudeQuota})
}

// claudeQuotaRouting serves routing from stored headers alone.
type claudeQuotaRouting struct{ reader claudequota.Reader }

func (r claudeQuotaRouting) QuotaStatus(authID, model string) auth.QuotaStatus {
	status := r.reader.Status(authID, model)
	return auth.QuotaStatus{
		Known:       status.Known,
		Available:   status.OK,
		ResetAt:     status.ResetAt,
		HeadroomPct: status.HeadroomPct,
		DrainBy:     status.DrainBy,
	}
}

// claudeQuotaCredential selects Claude OAuth credentials (API-key entries have
// no plan limits) and copies their routing-relevant state.
func claudeQuotaCredential(a *auth.Auth) (claudequota.Credential, bool) {
	if a == nil || !strings.EqualFold(strings.TrimSpace(a.Provider), "claude") {
		return claudequota.Credential{}, false
	}
	if a.Attributes != nil && strings.TrimSpace(a.Attributes["api_key"]) != "" {
		return claudequota.Credential{}, false
	}
	if claudeauth.ReadMetadataString(&a.Metadata, "access_token") == "" && claudeauth.ReadMetadataString(&a.Metadata, "refresh_token") == "" {
		return claudequota.Credential{}, false
	}
	name := strings.TrimSpace(a.FileName)
	if name == "" {
		name = a.ID
	}
	cred := claudequota.Credential{
		ID:       a.ID,
		Index:    a.EnsureIndex(),
		Name:     name,
		Label:    a.Label,
		Email:    strings.TrimSpace(claudeauth.ReadMetadataString(&a.Metadata, "email")),
		Disabled: a.Disabled || a.Status == auth.StatusDisabled,
	}
	if a.Quota.Exceeded {
		cred.CooldownUntil = a.Quota.NextRecoverAt
	}
	if len(a.Quota.Signals) > 0 {
		cred.Observations = append(cred.Observations, claudequota.Observation{Signals: a.Quota.Signals, ObservedAt: a.Quota.ObservedAt})
	}
	for model, state := range a.ModelStates {
		if state == nil {
			continue
		}
		// Model states keep the last snapshot of their own traffic, so a scoped
		// window survives responses to other models that omit it.
		if len(state.Quota.Signals) > 0 {
			cred.Observations = append(cred.Observations, claudequota.Observation{Signals: state.Quota.Signals, ObservedAt: state.Quota.ObservedAt})
		}
		if !state.Quota.Exceeded {
			continue
		}
		if cred.ModelCooldowns == nil {
			cred.ModelCooldowns = map[string]time.Time{}
		}
		cred.ModelCooldowns[model] = state.Quota.NextRecoverAt
	}
	return cred, true
}
