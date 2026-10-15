package auth

import (
	"context"
	"math"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// QuotaStatus is a credential's cached plan-limit standing for one model.
type QuotaStatus struct {
	// Tracked reports a credential the provider has plan limits for; Known
	// implies it. Untracked ones serve only when no tracked one has quota.
	Tracked bool
	// Known is false without usable data; routing then treats the credential
	// as available.
	Known bool
	// Available reports quota for the model; meaningful only when Known.
	Available bool
	// ResetAt is when an unavailable credential can serve the model again;
	// zero when unreported.
	ResetAt time.Time
	// HeadroomPct is the percent left in the window that caps the model;
	// meaningful only when Known and Available.
	HeadroomPct float64
	// DrainBy is when that window resets; zero when unreported.
	DrainBy time.Time
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

// drainHorizon stands in for an unreported reset: far enough that the
// credential neither starves nor outranks those with a reset due.
const drainHorizon = 7 * 24 * time.Hour

// QuotaFirstSelector drains credentials so none leaves quota unused at its
// reset. Among those with quota for the model it picks the highest pressure,
// headroom per hour until the capping window resets (at least one hour):
// equal resets favour more headroom, a sooner reset drains first. Ties
// round-robin. When every candidate is known exhausted it picks the one whose
// block ends soonest. Without a quota lookup it is plain round-robin.
type QuotaFirstSelector struct {
	rr RoundRobinSelector
}

// Pick implements Selector.
func (s *QuotaFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if ctx != nil {
		if lookup, _ := ctx.Value(quotaLookupKey{}).(quotaLookup); lookup != nil {
			available = quotaPreferredAuths(available, lookup, now)
		}
	}
	return s.rr.next(provider+":"+canonicalModelKey(model), available), nil
}

// drainPressure is headroom per hour left before the capping window resets.
func drainPressure(status QuotaStatus, now time.Time) float64 {
	left := drainHorizon
	if !status.DrainBy.IsZero() {
		left = status.DrainBy.Sub(now)
	}
	return status.HeadroomPct / math.Max(left.Hours(), 1)
}

// quotaPreferredAuths narrows candidates to their best quota tier, keeping
// order so rotation continues within it. Tiers: top pressure plus tracked
// unknowns, then untracked, then the soonest-ending exhausted. Tracked
// unknowns tie the leader: their first response reports their quota, so
// ranking them lower would starve them of the traffic that makes them known.
func quotaPreferredAuths(available []*Auth, lookup quotaLookup, now time.Time) []*Auth {
	statuses := make([]QuotaStatus, len(available))
	pressures := make([]float64, len(available))
	best := math.Inf(-1)
	for i, auth := range available {
		statuses[i] = lookup(auth)
		if statuses[i].Known && statuses[i].Available {
			pressures[i] = drainPressure(statuses[i], now)
			best = math.Max(best, pressures[i])
		}
	}
	var preferred, untracked, soonest []*Auth
	var soonestAt time.Time
	for i, auth := range available {
		status := statuses[i]
		switch {
		case !status.Known && !status.Tracked:
			untracked = append(untracked, auth)
		case !status.Known || (status.Available && pressures[i] == best):
			preferred = append(preferred, auth)
		case status.Available: // below the leader, never in the exhausted tier
		case len(soonest) == 0 || resetsEarlier(status.ResetAt, soonestAt):
			soonest, soonestAt = []*Auth{auth}, status.ResetAt
		case status.ResetAt.Equal(soonestAt):
			soonest = append(soonest, auth)
		}
	}
	switch {
	case len(preferred) > 0:
		return preferred
	case len(untracked) > 0:
		return untracked
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
