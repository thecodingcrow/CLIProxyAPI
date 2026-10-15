package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/claudequota"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// guardExternalDials fails the test on any DNS resolution: every real upstream
// (api.anthropic.com, GitHub) is a hostname, the fake upstream is 127.0.0.1.
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

// fakeUsageUpstream replays a recorded usage body per bearer token and counts requests.
type fakeUsageUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	counts map[string]int
}

func newFakeUsageUpstream(t *testing.T, bodies map[string]string) *fakeUsageUpstream {
	t.Helper()
	f := &fakeUsageUpstream{counts: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.counts[token]++
		f.mu.Unlock()
		fixture, ok := bodies[token]
		if r.URL.Path != "/api/oauth/usage" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || !ok {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		body, err := os.ReadFile(filepath.Join("..", "claudequota", "testdata", fixture))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.Close)
	if host, _, _ := net.SplitHostPort(strings.TrimPrefix(f.URL, "http://")); net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatalf("fake upstream %s is not loopback", f.URL)
	}
	return f
}

func (f *fakeUsageUpstream) count(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[token]
}

func (f *fakeUsageUpstream) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, count := range f.counts {
		total += count
	}
	return total
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
	store   *claudequota.Store
	clock   *quotaClock
}

// newQuotaTestServer wires the real management handler, v8 routes and store
// over two synthetic Claude OAuth credentials holding fake tokens.
func newQuotaTestServer(t *testing.T, usageQuota string, managementEnabled bool) *quotaTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "config-version: 8\nmanagement: {secret-key: test-password}\nupstream:\n  claude:\n    usage-quota: " + usageQuota + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
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
		{ID: "exhausted", FileName: "claude-exhausted.json", Provider: "claude", Metadata: map[string]any{"type": "claude", "access_token": "fake-exhausted", "email": "exhausted@example.test"}},
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
	clock := &quotaClock{now: time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC)}
	s.attachClaudeQuota(manager, claudequota.Options{
		Now:   clock.Now,
		Pause: func(context.Context, time.Duration) bool { return true },
	})
	s.managementRoutesEnabled.Store(managementEnabled)
	s.registerManagementRoutes()
	return &quotaTestServer{server: s, manager: manager, store: s.claudeQuota, clock: clock}
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

func (q *quotaTestServer) tick() {
	<-q.store.Tick(context.Background())
}

func usageQuotaYAML(upstream *fakeUsageUpstream) string {
	return "{enabled: true, base-url: " + upstream.URL + "}"
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

func TestCredentialsQuotaEndpointServesCachedLimits(t *testing.T) {
	guardExternalDials(t)
	upstream := newFakeUsageUpstream(t, map[string]string{"fake-healthy": "usage-healthy.json", "fake-exhausted": "usage-weekly-exhausted.json"})
	q := newQuotaTestServer(t, usageQuotaYAML(upstream), true)

	if code, _, body := q.get(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("GET without key = %d %s, want 401", code, body)
	}

	q.tick()
	code, boot, body := q.get(t, "test-password")
	if code != http.StatusOK || !boot.Enabled || len(boot.Accounts) != 3 || boot.Summary.Accounts.Pending != 2 {
		t.Fatalf("GET at boot = %d %s, want enabled with 2 pending accounts plus the disabled one", code, body)
	}
	if got := upstream.total(); got != 0 {
		t.Fatalf("upstream requests at boot = %d, want 0 (first poll waits one interval)", got)
	}

	q.clock.Advance(claudequota.DefaultRefreshInterval)
	q.tick()
	if upstream.count("fake-healthy") != 1 || upstream.count("fake-exhausted") != 1 || upstream.total() != 2 {
		t.Fatalf("upstream requests after one interval = %v, want one per enabled OAuth credential", upstream.counts)
	}

	_, first, _ := q.get(t, "test-password")
	code, snap, body := q.get(t, "test-password")
	if code != http.StatusOK || upstream.total() != 2 {
		t.Fatalf("two rapid GETs = %d, upstream total %d, want 200 and still 2 requests", code, upstream.total())
	}
	if !first.GeneratedAt.Equal(snap.GeneratedAt) {
		t.Fatalf("generated_at moved between reads without a clock change: %v vs %v", first.GeneratedAt, snap.GeneratedAt)
	}
	t.Logf("GET /v8/management/credentials/quota:\n%s", body)

	healthy := snapshotAccount(t, snap, "healthy")
	if healthy.State != claudequota.StateOK || healthy.AuthIndex == "" || healthy.Email != "healthy@example.test" || !healthy.HasQuota.Scoped["Fable"] {
		t.Fatalf("healthy = %+v, want ok, auth_index, email and Fable quota", healthy)
	}
	exhausted := snapshotAccount(t, snap, "exhausted")
	if exhausted.HasQuota.Any || exhausted.HasQuota.Scoped["Fable"] || exhausted.WeeklyScoped["Fable"].Percent != 40 {
		t.Fatalf("exhausted = %+v, want no quota although Fable scoped is 40", exhausted)
	}
	if disabled := snapshotAccount(t, snap, "disabled"); !disabled.Disabled {
		t.Fatalf("disabled = %+v, want disabled flag", disabled)
	}
	sum := snap.Summary
	if sum.Accounts.Total != 2 || sum.Accounts.Disabled != 1 || *sum.WeeklyAllUsedPct != 54.5 || *sum.SessionUsedPct != 15.5 || sum.Scoped["Fable"].RemainingPct != 45.5 {
		t.Fatalf("summary = %+v, want 2 enabled (1 disabled), weekly 54.5, session 15.5, Fable remaining 45.5", sum)
	}
	if st := q.store.Status("exhausted", "claude-fable-5"); st.OK || !st.Known {
		t.Fatalf("HasQuota(exhausted, fable) = %v/%v, want false/true", st.OK, st.Known)
	}

	q.clock.Advance(claudequota.DefaultRefreshInterval - time.Second)
	q.tick()
	if got := upstream.total(); got != 2 {
		t.Fatalf("upstream requests before the next interval = %d, want 2", got)
	}
	q.clock.Advance(time.Second)
	q.tick()
	if got := upstream.total(); got != 4 {
		t.Fatalf("upstream requests after the second interval = %d, want 4", got)
	}
}

func TestCredentialsQuotaEndpointDisabledByDefault(t *testing.T) {
	guardExternalDials(t)
	upstream := newFakeUsageUpstream(t, map[string]string{"fake-healthy": "usage-healthy.json"})
	q := newQuotaTestServer(t, "{base-url: "+upstream.URL+"}", true)

	for i := 0; i < 5; i++ {
		q.tick()
		q.clock.Advance(time.Hour)
	}
	code, snap, body := q.get(t, "test-password")
	if code != http.StatusOK || snap.Enabled || len(snap.Accounts) != 0 || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("GET with polling off = %d %s, want explicit enabled:false and no accounts", code, body)
	}
	if got := upstream.total(); got != 0 {
		t.Fatalf("upstream requests with polling off = %d, want 0", got)
	}
}

func TestCredentialsQuotaRouteAbsentWithoutManagementKey(t *testing.T) {
	guardExternalDials(t)
	upstream := newFakeUsageUpstream(t, nil)
	q := newQuotaTestServer(t, usageQuotaYAML(upstream), false)

	if code, _, body := q.get(t, "test-password"); code != http.StatusNotFound {
		t.Fatalf("GET with management disabled = %d %s, want 404", code, body)
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

// serve sends n Fable requests through the manager and returns the serving credentials.
func (e *recordingClaudeExecutor) serve(t *testing.T, manager *auth.Manager, n int) []string {
	t.Helper()
	e.mu.Lock()
	e.served = nil
	e.mu.Unlock()
	for i := 0; i < n; i++ {
		if _, err := manager.Execute(context.Background(), []string{"claude"}, coreexecutor.Request{Model: "claude-fable-5"}, coreexecutor.Options{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.served...)
}

func TestQuotaFirstRoutingSendsFableOnlyToTheCredentialWithQuota(t *testing.T) {
	guardExternalDials(t)
	upstream := newFakeUsageUpstream(t, map[string]string{"fake-healthy": "usage-healthy.json", "fake-exhausted": "usage-weekly-exhausted.json"})
	q := newQuotaTestServer(t, usageQuotaYAML(upstream), true)
	q.manager.SetSelector(&auth.QuotaFirstSelector{})
	executor := &recordingClaudeExecutor{}
	q.manager.RegisterExecutor(executor)
	for _, id := range []string{"healthy", "exhausted"} {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "claude-fable-5"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}

	q.tick()
	if got := executor.serve(t, q.manager, 4); strings.Join(got, ",") != "exhausted,healthy,exhausted,healthy" {
		t.Fatalf("served before the first poll = %v, want round-robin over both", got)
	}

	q.clock.Advance(claudequota.DefaultRefreshInterval)
	q.tick()
	before := upstream.total()
	if got := executor.serve(t, q.manager, 4); strings.Join(got, ",") != "healthy,healthy,healthy,healthy" {
		t.Fatalf("served after the poll = %v, want only healthy (exhausted is at weekly_all 100 despite Fable 40)", got)
	}
	if got := upstream.total(); got != before {
		t.Fatalf("usage requests during routing = %d, want none", got-before)
	}
}

func TestDialGuardCatchesAnthropicHost(t *testing.T) {
	restore := installDialGuard()
	target, _ := url.Parse(claudequota.UsageEndpoint(&config.Config{}))
	_, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", target.Host+":443")
	if attempts := restore(); err == nil || len(attempts) == 0 {
		t.Fatalf("dial %s: err=%v attempts=%v, want the guard to refuse it", target.Host, err, attempts)
	}
}
