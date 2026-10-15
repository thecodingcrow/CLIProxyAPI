package auth

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// QuotaStatus is a credential's cached plan-limit standing for one model.
type QuotaStatus struct {
	// Known is false without usable data; routing then treats the credential
	// as available.
	Known bool
	// Available reports quota for the model; meaningful only when Known.
	Available bool
	// ResetAt is when an unavailable credential can serve the model again;
	// zero when unreported.
	ResetAt time.Time
}

// QuotaProvider reports cached plan-limit standing. Implementations answer
// from memory: selection calls it per candidate on every request.
type QuotaProvider interface {
	QuotaStatus(authID, model string) QuotaStatus
}

type quotaProviderHolder struct{ provider QuotaProvider }

// SetQuotaProvider installs the plan-limit source QuotaFirstSelector ranks by;
// nil removes it.
func (m *Manager) SetQuotaProvider(provider QuotaProvider) {
	if m == nil {
		return
	}
	m.quotaProvider.Store(&quotaProviderHolder{provider: provider})
}

// quotaLookup judges a candidate for the request's model as that credential
// sends it upstream.
type quotaLookup func(auth *Auth) QuotaStatus

type quotaLookupKey struct{}

// withQuotaLookup hands a quota-ranking selector the lookup for routeModel.
func (m *Manager) withQuotaLookup(ctx context.Context, selector Selector, routeModel string) context.Context {
	if !ranksByQuota(selector) {
		return ctx
	}
	holder := m.quotaProvider.Load()
	if holder == nil || holder.provider == nil {
		return ctx
	}
	provider := holder.provider
	return context.WithValue(ctx, quotaLookupKey{}, quotaLookup(func(auth *Auth) QuotaStatus {
		return provider.QuotaStatus(auth.ID, m.selectionModelKeyForAuth(auth, routeModel))
	}))
}

func ranksByQuota(selector Selector) bool {
	switch s := selector.(type) {
	case *QuotaFirstSelector:
		return true
	case *SessionAffinitySelector:
		_, ok := s.fallback.(*QuotaFirstSelector)
		return ok
	default:
		return false
	}
}

// QuotaFirstSelector is round-robin over the credentials still worth sending
// to: those with plan quota for the model or without quota data. When every
// candidate is known exhausted it picks the one whose block ends soonest.
// Without a quota lookup it is plain round-robin.
type QuotaFirstSelector struct {
	rr RoundRobinSelector
}

// Pick implements Selector.
func (s *QuotaFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, time.Now())
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if lookup, _ := ctx.Value(quotaLookupKey{}).(quotaLookup); lookup != nil {
		available = quotaPreferredAuths(available, lookup)
	}
	return s.rr.next(provider+":"+canonicalModelKey(model), available), nil
}

// quotaPreferredAuths narrows candidates to their best quota tier, keeping
// order so rotation continues within it. Unknown ranks with available because
// missing data must never starve a credential.
func quotaPreferredAuths(available []*Auth, lookup quotaLookup) []*Auth {
	var preferred, soonest []*Auth
	var soonestAt time.Time
	for _, auth := range available {
		status := lookup(auth)
		if !status.Known || status.Available {
			preferred = append(preferred, auth)
			continue
		}
		if len(preferred) > 0 {
			continue
		}
		switch {
		case len(soonest) == 0 || resetsEarlier(status.ResetAt, soonestAt):
			soonest, soonestAt = []*Auth{auth}, status.ResetAt
		case status.ResetAt.Equal(soonestAt):
			soonest = append(soonest, auth)
		}
	}
	if len(preferred) > 0 {
		return preferred
	}
	return soonest
}

// resetsEarlier orders reset times with an unreported (zero) one last.
func resetsEarlier(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	return b.IsZero() || a.Before(b)
}
