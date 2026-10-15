package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// guardExternalDials fails the test on any DNS resolution: every real upstream
// (api.anthropic.com, GitHub) is a hostname.
func guardExternalDials(t *testing.T) {
	t.Helper()
	restore := installDialGuard()
	t.Cleanup(func() {
		if attempts := restore(); len(attempts) > 0 {
			t.Errorf("test attempted external resolution: %v", attempts)
		}
	})
}

// installDialGuard refuses every resolver dial; restore reinstates the
// previous resolver and returns the refused attempts.
func installDialGuard() (restore func() []string) {
	previous := net.DefaultResolver
	var mu sync.Mutex
	var attempts []string
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(_ context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		attempts = append(attempts, network+" "+address)
		mu.Unlock()
		return nil, errors.New("external dial blocked by test guard")
	}}
	return func() []string {
		net.DefaultResolver = previous
		mu.Lock()
		defer mu.Unlock()
		return attempts
	}
}

type quotaClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *quotaClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *quotaClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type quotaTestServer struct {
	server  *Server
	manager *auth.Manager
	clock   *quotaClock
}

// newQuotaTestServer wires the real management handler, v8 routes and store
// over synthetic Claude OAuth credentials holding fake tokens.
func newQuotaTestServer(t *testing.T, managementEnabled bool) *quotaTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\nmanagement: {secret-key: test-password}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuthDir = t.TempDir()

	manager := auth.NewManager(nil, nil, nil)
	for _, a := range []*auth.Auth{
		{ID: "healthy", FileName: "claude-healthy.json", Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": "fake-healthy", "email": "healthy@example.test"}},
		{ID: "fable-out", FileName: "claude-fable-out.json", Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": "fake-fable-out"}},
		{ID: "disabled", FileName: "claude-disabled.json", Provider: "claude", Disabled: true, Status: auth.StatusDisabled, Metadata: map[string]any{"type": "claude", "access_token": "fake-disabled"}},
		{ID: "claude-key", Provider: "claude", Attributes: map[string]string{"api_key": "fake-key"}},
		{ID: "codex", FileName: "codex.json", Provider: "codex", Metadata: map[string]any{"access_token": "fake-codex"}},
	} {
		if _, errRegister := manager.Register(auth.WithSkipPersist(context.Background()), a); errRegister != nil {
			t.Fatalf("register %s: %v", a.ID, errRegister)
		}
	}

	h := management.NewHandler(cfg, path, manager)
	h.SetLocalPassword("test-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	clock := &quotaClock{now: time.Now()}
	s.attachClaudeQuota(manager, clock.Now)
	s.managementRoutesEnabled.Store(managementEnabled)
	s.registerManagementRoutes()
	return &quotaTestServer{server: s, manager: manager, clock: clock}
}

// respond records one Claude response carrying headers for id, through the
// manager's own observation path.
func (q *quotaTestServer) respond(id, model string, headers http.Header) {
	ctx := internallogging.WithResponseHeadersHolder(auth.WithSkipPersist(context.Background()))
	internallogging.SetResponseHeaders(ctx, headers)
	q.manager.MarkResult(ctx, auth.Result{AuthID: id, Provider: "claude", Model: model, Success: true})
}

func (q *quotaTestServer) get(t *testing.T, key string) (int, claudequota.Snapshot, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v8/management/credentials/quota", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	q.server.engine.ServeHTTP(recorder, req)
	var snap claudequota.Snapshot
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &snap); err != nil {
			t.Fatalf("decode body %s: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, snap, recorder.Body.String()
}

func snapshotAccount(t *testing.T, snap claudequota.Snapshot, id string) claudequota.Account {
	t.Helper()
	for _, account := range snap.Accounts {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %q missing from %+v", id, snap.Accounts)
	return claudequota.Account{}
}

// unifiedHeaders builds a measured-shape response: 5h, 7d and the Fable 7d_oi window.
func unifiedHeaders(now time.Time, util5h, util7d, util7dOI, status7dOI string) http.Header {
	reset := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	unified := "allowed"
	if status7dOI == "rejected" {
		unified = "rejected"
	}
	// Lower-case wire names, canonicalised so later Set/Del address the same key.
	h := http.Header{}
	for name, values := range map[string][]string{
		"anthropic-ratelimit-unified-status":               {unified},
		"anthropic-ratelimit-unified-representative-claim": {"five_hour"},
		"anthropic-ratelimit-unified-5h-status":            {"allowed"},
		"anthropic-ratelimit-unified-5h-utilization":       {util5h},
		"anthropic-ratelimit-unified-5h-reset":             {reset(3 * time.Hour)},
		"anthropic-ratelimit-unified-7d-status":            {"allowed"},
		"anthropic-ratelimit-unified-7d-utilization":       {util7d},
		"anthropic-ratelimit-unified-7d-reset":             {reset(72 * time.Hour)},
		"anthropic-ratelimit-unified-7d_oi-status":         {status7dOI},
		"anthropic-ratelimit-unified-7d_oi-utilization":    {util7dOI},
		"anthropic-ratelimit-unified-7d_oi-reset":          {reset(48 * time.Hour)},
		"anthropic-ratelimit-unified-reset":                {reset(3 * time.Hour)},
	} {
		if values[0] != "" {
			h[http.CanonicalHeaderKey(name)] = values
		}
	}
	return h
}

func TestCredentialsQuotaEndpointServesObservedHeaders(t *testing.T) {
	guardExternalDials(t)
	q := newQuotaTestServer(t, true)

	if code, _, body := q.get(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("GET without key = %d %s, want 401", code, body)
	}
	code, boot, body := q.get(t, "test-password")
	if code != http.StatusOK || !boot.Enabled || len(boot.Accounts) != 3 || boot.Summary.Accounts.Pending != 2 || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("GET before traffic = %d %s, want enabled with 2 pending accounts plus the disabled one", code, body)
	}

	q.respond("healthy", "claude-sonnet-4-5", unifiedHeaders(q.clock.Now(), "0.10", "0.55", "0.40", "allowed"))
	code, snap, body := q.get(t, "test-password")
	t.Logf("GET /v8/management/credentials/quota:\n%s", body)
	healthy := snapshotAccount(t, snap, "healthy")
	if code != http.StatusOK || healthy.State != claudequota.StateOK || healthy.Stale || healthy.ObservedAt == nil || healthy.Email != "healthy@example.test" {
		t.Fatalf("healthy = %+v, want ok, fresh, observed_at and email", healthy)
	}
	if healthy.Session.Percent != 10 || healthy.WeeklyAll.Percent != 55 || healthy.WeeklyScoped["Fable"].Percent != 40 || !healthy.HasQuota.Any || !healthy.HasQuota.Scoped["Fable"] {
		t.Fatalf("healthy limits = %+v / %+v / %+v, has_quota %+v", healthy.Session, healthy.WeeklyAll, healthy.WeeklyScoped, healthy.HasQuota)
	}
	// remaining = 100 - max(Fable 40, weekly_all 55).
	sum := snap.Summary
	if *sum.WeeklyAllUsedPct != 55 || *sum.SessionUsedPct != 10 || sum.Scoped["Fable"].UsedPct != 55 || sum.Scoped["Fable"].RemainingPct != 45 || sum.Accounts.Pending != 1 {
		t.Fatalf("summary = %+v, want weekly 55, session 10, Fable used 55 remaining 45, 1 pending", sum)
	}
	if pending := snapshotAccount(t, snap, "fable-out"); pending.State != claudequota.StatePending || pending.HasQuota != nil {
		t.Fatalf("credential without signals = %+v, want pending", pending)
	}
	if st := q.server.claudeQuota.Status("fable-out", "claude-fable-5"); st.Known || !st.OK {
		t.Fatalf("Status without signals = %+v, want unknown and available", st)
	}
}

func TestCredentialsQuotaFableRejectionBlocksOnlyFable(t *testing.T) {
	guardExternalDials(t)
	q := newQuotaTestServer(t, true)
	q.respond("fable-out", "claude-fable-5", unifiedHeaders(q.clock.Now(), "0.10", "0.55", "1.02", "rejected"))
	// A later unscoped response omits 7d_oi; the Fable model's own snapshot keeps the rejection.
	later := unifiedHeaders(q.clock.Now(), "0.11", "0.56", "", "")
	later.Del("anthropic-ratelimit-unified-7d_oi-reset")
	q.respond("fable-out", "claude-sonnet-4-5", later)

	store := q.server.claudeQuota
	if st := store.Status("fable-out", "claude-fable-5"); !st.Known || st.OK || st.ResetAt.IsZero() {
		t.Fatalf("Status(fable) = %+v, want known, blocked, with reset", st)
	}
	if st := store.Status("fable-out", "claude-sonnet-4-5"); !st.Known || !st.OK {
		t.Fatalf("Status(sonnet) = %+v, want known and available", st)
	}
	_, snap, _ := q.get(t, "test-password")
	account := snapshotAccount(t, snap, "fable-out")
	if !account.HasQuota.Any || account.HasQuota.Scoped["Fable"] || account.WeeklyAll.Percent != 56 {
		t.Fatalf("account = %+v / %+v, want unscoped quota, no Fable quota, weekly from the newer response", account.HasQuota, account.WeeklyAll)
	}
}

func TestCredentialsQuotaWindowPastItsResetIsNotExhausted(t *testing.T) {
	guardExternalDials(t)
	q := newQuotaTestServer(t, true)
	h := unifiedHeaders(q.clock.Now(), "0.80", "1.0", "0.40", "allowed")
	h.Set("anthropic-ratelimit-unified-7d-status", "rejected")
	inAnHour := strconv.FormatInt(q.clock.Now().Add(time.Hour).Unix(), 10)
	for _, window := range []string{"5h", "7d", "7d_oi"} {
		h.Set("anthropic-ratelimit-unified-"+window+"-reset", inAnHour)
	}
	q.respond("healthy", "claude-sonnet-4-5", h)
	if st := q.server.claudeQuota.Status("healthy", "claude-sonnet-4-5"); st.OK {
		t.Fatalf("Status before the 7d reset = %+v, want blocked", st)
	}

	q.clock.Advance(2 * time.Hour)
	if st := q.server.claudeQuota.Status("healthy", "claude-sonnet-4-5"); !st.Known || !st.OK {
		t.Fatalf("Status after the 7d reset = %+v, want known and available", st)
	}
	_, snap, _ := q.get(t, "test-password")
	account := snapshotAccount(t, snap, "healthy")
	if !account.Stale || !account.HasQuota.Any || account.WeeklyAll.Percent != 100 {
		t.Fatalf("account = %+v, want stale, with quota, raw weekly 100 kept", account)
	}
	// Reset windows count as 0% used in the summary.
	sum := snap.Summary
	if *sum.WeeklyAllUsedPct != 0 || *sum.SessionUsedPct != 0 || sum.Scoped["Fable"].UsedPct != 0 || sum.Scoped["Fable"].RemainingPct != 100 {
		t.Fatalf("summary = %+v, Fable %+v, want weekly 0, session 0, Fable used 0 remaining 100", sum, sum.Scoped["Fable"])
	}
}

func TestCredentialsQuotaRouteAbsentWithoutManagementKey(t *testing.T) {
	guardExternalDials(t)
	q := newQuotaTestServer(t, false)
	if code, _, body := q.get(t, "test-password"); code != http.StatusNotFound {
		t.Fatalf("GET with management disabled = %d %s, want 404", code, body)
	}
}

func TestClaudeQuotaStartsNoGoroutine(t *testing.T) {
	guardExternalDials(t)
	manager := auth.NewManager(nil, nil, nil)
	s := &Server{mgmt: management.NewHandler(&config.Config{}, "", manager)}
	before := runtime.NumGoroutine()
	s.attachClaudeQuota(manager, nil)
	s.claudeQuota.Snapshot()
	s.claudeQuota.Status("any", "claude-fable-5")
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines %d -> %d, want none started by the quota source", before, after)
	}
}

// recordingClaudeExecutor records which credential served each request and
// never leaves the process.
type recordingClaudeExecutor struct {
	mu     sync.Mutex
	served []string
}

func (e *recordingClaudeExecutor) Identifier() string { return "claude" }

func (e *recordingClaudeExecutor) Execute(_ context.Context, a *auth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.served = append(e.served, a.ID)
	return coreexecutor.Response{}, nil
}

func (e *recordingClaudeExecutor) ExecuteStream(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not used")
}

func (e *recordingClaudeExecutor) Refresh(_ context.Context, a *auth.Auth) (*auth.Auth, error) {
	return a, nil
}

func (e *recordingClaudeExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not used")
}

func (e *recordingClaudeExecutor) HttpRequest(context.Context, *auth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not used")
}

// serve sends n requests for model through the manager and returns the serving credentials.
func (e *recordingClaudeExecutor) serve(t *testing.T, manager *auth.Manager, model string, n int) []string {
	t.Helper()
	e.mu.Lock()
	e.served = nil
	e.mu.Unlock()
	for i := 0; i < n; i++ {
		if _, err := manager.Execute(context.Background(), []string{"claude"}, coreexecutor.Request{Model: model}, coreexecutor.Options{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.served...)
}

// newQuotaRoutingServer routes quota-first over the healthy and fable-out
// credentials, both serving Fable and Sonnet.
func newQuotaRoutingServer(t *testing.T) (*quotaTestServer, *recordingClaudeExecutor) {
	t.Helper()
	guardExternalDials(t)
	q := newQuotaTestServer(t, true)
	q.manager.SetSelector(&auth.QuotaFirstSelector{})
	executor := &recordingClaudeExecutor{}
	q.manager.RegisterExecutor(executor)
	for _, id := range []string{"healthy", "fable-out"} {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-fable-5"}, {ID: "claude-sonnet-4-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return q, executor
}

func assertServed(t *testing.T, label string, got []string, want string) {
	t.Helper()
	if strings.Join(got, ",") != want {
		t.Fatalf("%s = %v, want %s", label, got, want)
	}
}

func TestQuotaFirstRoutingDrainsTheCredentialWithMoreWeeklyHeadroom(t *testing.T) {
	q, executor := newQuotaRoutingServer(t)
	q.respond("healthy", "claude-sonnet-4-5", unifiedHeaders(q.clock.Now(), "0.10", "0.20", "0.10", "allowed"))
	q.respond("fable-out", "claude-sonnet-4-5", unifiedHeaders(q.clock.Now(), "0.10", "0.70", "0.10", "allowed"))
	assertServed(t, "served at 80 vs 30 headroom", executor.serve(t, q.manager, "claude-sonnet-4-5", 3), "healthy,healthy,healthy")

	q.respond("healthy", "claude-sonnet-4-5", unifiedHeaders(q.clock.Now(), "0.10", "0.71", "0.10", "allowed"))
	assertServed(t, "served once healthy drops below", executor.serve(t, q.manager, "claude-sonnet-4-5", 2), "fable-out,fable-out")
}

func TestQuotaFirstRoutingSendsFableToTheCredentialWithFableLeft(t *testing.T) {
	q, executor := newQuotaRoutingServer(t)
	q.respond("healthy", "claude-fable-5", unifiedHeaders(q.clock.Now(), "0.10", "0.60", "0.40", "allowed"))
	q.respond("fable-out", "claude-fable-5", unifiedHeaders(q.clock.Now(), "0.10", "0.30", "1.00", "rejected"))

	assertServed(t, "served Fable", executor.serve(t, q.manager, "claude-fable-5", 3), "healthy,healthy,healthy")
	// Sonnet sees weekly_all only: 70 left on fable-out against 40.
	assertServed(t, "served Sonnet", executor.serve(t, q.manager, "claude-sonnet-4-5", 3), "fable-out,fable-out,fable-out")
}

func TestQuotaFirstRoutingSkipsASpentSessionUntilItResets(t *testing.T) {
	for name, spend := range map[string]func(http.Header){
		"utilization 100": func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.00") },
		"rejected":        func(h http.Header) { h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected") },
	} {
		t.Run(name, func(t *testing.T) {
			q, executor := newQuotaRoutingServer(t)
			spent := unifiedHeaders(q.clock.Now(), "0.40", "0.20", "0.10", "allowed")
			spend(spent)
			q.respond("healthy", "claude-sonnet-4-5", spent)
			q.respond("fable-out", "claude-sonnet-4-5", unifiedHeaders(q.clock.Now(), "0.10", "0.70", "0.10", "allowed"))
			assertServed(t, "served while healthy's session is spent", executor.serve(t, q.manager, "claude-sonnet-4-5", 3), "fable-out,fable-out,fable-out")

			q.clock.Advance(3 * time.Hour) // the session reset unifiedHeaders reports
			assertServed(t, "served after the session reset", executor.serve(t, q.manager, "claude-sonnet-4-5", 2), "healthy,healthy")
		})
	}
}

func TestQuotaFirstRoutingKeepsTheAPIKeyOutWhileOAuthHasQuota(t *testing.T) {
	q, executor := newQuotaRoutingServer(t)
	registry.GetGlobalRegistry().RegisterClient("claude-key", "claude", []*registry.ModelInfo{{ID: "claude-sonnet-4-5"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("claude-key") })
	count := func(served []string, id string) int {
		n := 0
		for _, s := range served {
			if s == id {
				n++
			}
		}
		return n
	}

	q.respond("healthy", "claude-sonnet-4-5", unifiedHeaders(q.clock.Now(), "0.10", "0.20", "0.10", "allowed"))
	served := executor.serve(t, q.manager, "claude-sonnet-4-5", 100)
	if count(served, "claude-key") != 0 || count(served, "fable-out") == 0 {
		t.Fatalf("served with fable-out still unknown = %v, want fable-out probed and claude-key never", served)
	}

	q.respond("fable-out", "claude-sonnet-4-5", unifiedHeaders(q.clock.Now(), "0.10", "0.70", "0.10", "allowed"))
	if served := executor.serve(t, q.manager, "claude-sonnet-4-5", 100); count(served, "claude-key") != 0 {
		t.Fatalf("served with both OAuth known-good = %v, want claude-key 0 of 100", served)
	}
}

func TestQuotaFirstRoutingSendsFableOnlyToTheCredentialWithQuota(t *testing.T) {
	q, executor := newQuotaRoutingServer(t)

	if got := executor.serve(t, q.manager, "claude-fable-5", 4); strings.Join(got, ",") != "fable-out,healthy,fable-out,healthy" {
		t.Fatalf("served before any signal = %v, want round-robin over both", got)
	}

	q.respond("healthy", "claude-fable-5", unifiedHeaders(time.Now(), "0.10", "0.55", "0.40", "allowed"))
	q.respond("fable-out", "claude-fable-5", unifiedHeaders(time.Now(), "0.10", "0.55", "1.02", "rejected"))
	if got := executor.serve(t, q.manager, "claude-fable-5", 4); strings.Join(got, ",") != "healthy,healthy,healthy,healthy" {
		t.Fatalf("served Fable after signals = %v, want only healthy", got)
	}
	if got := executor.serve(t, q.manager, "claude-sonnet-4-5", 4); !strings.Contains(strings.Join(got, ","), "fable-out") {
		t.Fatalf("served sonnet after signals = %v, want fable-out still in rotation", got)
	}
}

func TestDialGuardCatchesAnthropicHost(t *testing.T) {
	restore := installDialGuard()
	_, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", "api.anthropic.com:443")
	if attempts := restore(); err == nil || len(attempts) == 0 {
		t.Fatalf("dial api.anthropic.com: err=%v attempts=%v, want the guard to refuse it", err, attempts)
	}
}
