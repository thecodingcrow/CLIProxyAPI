package modelusage

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	mainSession  = "11111111-1111-4111-8111-111111111111"
	childSession = "22222222-2222-4222-8222-222222222222"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock {
	// Bucket-aligned start keeps advance arithmetic exact.
	return &fakeClock{now: time.Unix(BucketSeconds*2_900_000, 0)}
}

func record(provider, model string, input, output int64) coreusage.Record {
	return coreusage.Record{
		Provider: provider,
		Model:    model,
		Detail:   coreusage.Detail{InputTokens: input, OutputTokens: output, TotalTokens: input + output},
	}
}

func findRow(t *testing.T, snap Snapshot, provider, model string, subagent bool) Row {
	t.Helper()
	for _, row := range snap.Models {
		if row.Provider == provider && row.Model == model && row.Subagent == subagent {
			return row
		}
	}
	t.Fatalf("row %s/%s subagent=%v missing in %+v", provider, model, subagent, snap.Models)
	return Row{}
}

func TestSnapshotDropsDataOlderThanWindow(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	agg.HandleUsage(context.Background(), record("claude", "old", 10, 5))
	clock.Advance(time.Duration(BucketSeconds) * time.Second)
	agg.HandleUsage(context.Background(), record("claude", "old", 1, 1))

	clock.Advance(time.Duration(BucketSeconds*(bucketCount-2)) * time.Second)
	if got := findRow(t, agg.Snapshot(), "claude", "old", false); got.Requests != 2 || got.TotalTokens != 17 {
		t.Fatalf("last in-window bucket: got %+v, want 2 requests / 17 tokens", got)
	}

	clock.Advance(time.Duration(BucketSeconds) * time.Second)
	if got := findRow(t, agg.Snapshot(), "claude", "old", false); got.Requests != 1 || got.TotalTokens != 2 {
		t.Fatalf("after first bucket expired: got %+v, want 1 request / 2 tokens", got)
	}

	clock.Advance(time.Duration(BucketSeconds) * time.Second)
	if snap := agg.Snapshot(); len(snap.Models) != 0 {
		t.Fatalf("after window passed: got %+v, want no rows", snap.Models)
	}
}

func TestBucketReuseAfterFullRotationResetsCounters(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	agg.HandleUsage(context.Background(), record("claude", "m", 100, 0))
	clock.Advance(time.Duration(BucketSeconds*bucketCount) * time.Second)
	agg.HandleUsage(context.Background(), record("claude", "m", 1, 0))
	if got := findRow(t, agg.Snapshot(), "claude", "m", false); got.Requests != 1 || got.InputTokens != 1 {
		t.Fatalf("got %+v, want stale ring slot reset", got)
	}
}

func TestSubagentSplit(t *testing.T) {
	agg := New(newClock().Now, 0)
	main := record("claude", "claude-opus-5-5", 10, 1)
	main.SessionID = mainSession
	agg.HandleUsage(context.Background(), main)

	childCtx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{
		SessionID:       childSession,
		ParentSessionID: mainSession,
	})
	agg.HandleUsage(childCtx, record("claude", "claude-opus-5-5", 20, 2))

	self := record("claude", "claude-opus-5-5", 1, 1)
	self.SessionID, self.ParentSessionID = mainSession, mainSession
	agg.HandleUsage(context.Background(), self)

	snap := agg.Snapshot()
	if len(snap.Models) != 2 {
		t.Fatalf("got %d rows, want main + subagent: %+v", len(snap.Models), snap.Models)
	}
	if got := findRow(t, snap, "claude", "claude-opus-5-5", false); got.Requests != 2 || got.TotalTokens != 13 {
		t.Fatalf("main row: %+v", got)
	}
	child := findRow(t, snap, "claude", "claude-opus-5-5", true)
	if child.Requests != 1 || child.InputTokens != 20 {
		t.Fatalf("subagent row: %+v", child)
	}
	if snap.Models[0].Subagent != true {
		t.Fatalf("rows not sorted by total_tokens desc: %+v", snap.Models)
	}
}

func TestFailedCounted(t *testing.T) {
	agg := New(newClock().Now, 0)
	failed := record("codex", "gpt-5", 0, 0)
	failed.Failed = true
	agg.HandleUsage(context.Background(), failed)
	agg.HandleUsage(context.Background(), record("codex", "gpt-5", 3, 4))
	if got := findRow(t, agg.Snapshot(), "codex", "gpt-5", false); got.Requests != 2 || got.Failed != 1 {
		t.Fatalf("got %+v, want 2 requests / 1 failed", got)
	}
}

// Each provider reports raw usage differently; rows must carry the normalised meaning
// (input includes cached, output includes reasoning, total = input + output).
func TestTokensNormalisedAcrossProviders(t *testing.T) {
	for _, tc := range []struct {
		name, provider                          string
		detail                                  coreusage.Detail
		input, output, cached, reasoning, total int64
	}{
		{
			// Claude input_tokens is uncached only; cache read and creation are separate.
			name: "claude", provider: "claude",
			detail: coreusage.Detail{InputTokens: 5, CacheReadTokens: 7, CacheCreationTokens: 3, OutputTokens: 5},
			input:  15, output: 5, cached: 10, total: 20,
		},
		{
			// OpenAI prompt_tokens already include cached tokens.
			name: "openai", provider: "codex",
			detail: coreusage.Detail{InputTokens: 15, CacheReadTokens: 10, CachedTokens: 10, OutputTokens: 5, TotalTokens: 20},
			input:  15, output: 5, cached: 10, total: 20,
		},
		{
			// Gemini candidates exclude thoughts.
			name: "gemini", provider: "gemini",
			detail: coreusage.Detail{InputTokens: 3, OutputTokens: 4, ReasoningTokens: 6, TotalTokens: 13},
			input:  3, output: 10, reasoning: 6, total: 13,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agg := New(newClock().Now, 0)
			agg.HandleUsage(context.Background(), coreusage.Record{Provider: tc.provider, Model: "m", Detail: tc.detail})
			got := findRow(t, agg.Snapshot(), tc.provider, "m", false)
			want := Counters{Requests: 1, InputTokens: tc.input, OutputTokens: tc.output, CachedTokens: tc.cached, ReasoningTokens: tc.reasoning, TotalTokens: tc.total}
			if got.Counters != want {
				t.Fatalf("got %+v, want %+v", got.Counters, want)
			}
		})
	}
}

func TestTokensFallBackToRawWhenUnclassified(t *testing.T) {
	agg := New(newClock().Now, 0)
	r := record("", "m", 4, 2)
	r.Detail.CacheReadTokens = 1
	agg.HandleUsage(context.Background(), r)
	got := findRow(t, agg.Snapshot(), "unknown", "m", false)
	if got.InputTokens != 4 || got.OutputTokens != 2 || got.CachedTokens != 1 || got.TotalTokens != 6 {
		t.Fatalf("got %+v, want raw fields", got.Counters)
	}
}

func TestProviderMatchesAPIKeyGrouping(t *testing.T) {
	agg := New(newClock().Now, 0)
	agg.HandleUsage(context.Background(), record("openai-compatible-OpenRouter", "kimi", 1, 1))
	agg.HandleUsage(context.Background(), record(" Claude ", "opus", 1, 1))
	agg.HandleUsage(context.Background(), record("", "x", 1, 1))
	snap := agg.Snapshot()
	findRow(t, snap, "openrouter", "kimi", false)
	findRow(t, snap, "claude", "opus", false)
	findRow(t, snap, "unknown", "x", false)
}

func TestKeyCapEvictsLeastRecentlyActive(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 2)
	agg.HandleUsage(context.Background(), record("claude", "a", 1, 0))
	clock.Advance(time.Duration(BucketSeconds) * time.Second)
	agg.HandleUsage(context.Background(), record("claude", "b", 1, 0))
	clock.Advance(time.Duration(BucketSeconds) * time.Second)
	agg.HandleUsage(context.Background(), record("claude", "a", 1, 0)) // refreshes a
	agg.HandleUsage(context.Background(), record("claude", "c", 1, 0)) // evicts b

	snap := agg.Snapshot()
	if len(snap.Models) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(snap.Models), snap.Models)
	}
	for _, row := range snap.Models {
		if row.Model == "b" {
			t.Fatalf("least recently active key b survived: %+v", snap.Models)
		}
	}
}

func TestSnapshotJSONContract(t *testing.T) {
	agg := New(newClock().Now, 0)
	raw, err := json.Marshal(agg.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["bucket_seconds"] != float64(600) || got["window_minutes"] != float64(200) {
		t.Fatalf("window fields: %s", raw)
	}
	if _, err := time.Parse(time.RFC3339, got["generated_at"].(string)); err != nil {
		t.Fatalf("generated_at: %v", err)
	}
	if models, ok := got["models"].([]any); !ok || len(models) != 0 {
		t.Fatalf("models must be an empty array, got %s", raw)
	}
}

func TestConcurrentHandleUsageAndSnapshot(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 16)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				agg.HandleUsage(context.Background(), record("claude", fmt.Sprintf("m%d", (w+i)%24), 1, 1))
				if i%50 == 0 {
					clock.Advance(time.Second)
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = agg.Snapshot()
			}
		}()
	}
	wg.Wait()
	if n := len(agg.Snapshot().Models); n == 0 || n > 16 {
		t.Fatalf("rows = %d, want 1..16 (key cap)", n)
	}
}
