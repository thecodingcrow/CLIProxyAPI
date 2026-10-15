package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaFirstRoutingStrategyHotReloads(t *testing.T) {
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	apply := func(sequence uint64, strategy string) coreauth.Selector {
		t.Helper()
		commit := configCommit{cfg: &internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: strategy}}, sequence: sequence}
		if !service.applyManagerConfig(context.Background(), commit) {
			t.Fatalf("applyManagerConfig(%q) failed", strategy)
		}
		return service.coreManager.Selector()
	}

	for i, strategy := range []string{"Quota-First", "quotafirst"} {
		if selector := apply(uint64(i+1), strategy); !isQuotaFirst(selector) {
			t.Fatalf("selector after reload to %q = %T, want *auth.QuotaFirstSelector", strategy, selector)
		}
	}
	if selector := apply(3, ""); !isRoundRobin(selector) {
		t.Fatalf("selector after reload to the default = %T, want *auth.RoundRobinSelector", selector)
	}
}

func isRoundRobin(selector coreauth.Selector) bool {
	_, ok := selector.(*coreauth.RoundRobinSelector)
	return ok
}

func isQuotaFirst(selector coreauth.Selector) bool {
	_, ok := selector.(*coreauth.QuotaFirstSelector)
	return ok
}
