package modelusage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// claudeRecord carries Anthropic usage: input_tokens excludes cache reads and writes.
func claudeRecord(model, authIndex string, input, cacheRead, cacheWrite, output int64) coreusage.Record {
	return coreusage.Record{
		Provider:  "claude",
		Model:     model,
		AuthIndex: authIndex,
		Detail: coreusage.Detail{
			InputTokens: input, CacheReadTokens: cacheRead, CachedTokens: cacheRead, CacheCreationTokens: cacheWrite,
			OutputTokens: output, TotalTokens: input + cacheRead + cacheWrite + output,
		},
	}
}

func TestRecordAddsExactCostPerModel(t *testing.T) {
	agg := New(newClock().Now, 0)
	ctx := context.Background()
	// 1,000 input + 20,000 cache read + 3,000 cache write (5m rate) + 500 output.
	for _, model := range []string{"claude-fable-5-1", "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		agg.HandleUsage(ctx, claudeRecord(model, "a", 1000, 20_000, 3000, 500))
	}
	agg.HandleUsage(ctx, claudeRecord("claude-opus-5-5-20261001", "a", 1000, 20_000, 3000, 500))
	agg.HandleUsage(ctx, record("codex", "gpt-5", 100, 10))
	agg.HandleUsage(ctx, claudeRecord("claude-opus-5-5", "a", 0, 0, 0, 0)) // no usage

	snap := agg.Snapshot(RangeWindow)
	for model, want := range map[string]int64{
		"claude-fable-5-1":         10_000 + 5000 + 37_500 + 25_000,
		"claude-sonnet-5-5":        2000 + 2000 + 7500 + 5000,
		"claude-haiku-5-5":         100 + 200 + 375 + 250,
		"claude-opus-5-5-20261001": 4000 + 4000 + 15_000 + 10_000,
	} {
		row := findRow(t, snap, "claude", model, false)
		if row.CostUSDMicros != want || row.UnpricedRequests != 0 || row.CacheWriteTokens != 3000 || row.CostPartial {
			t.Errorf("%s: %+v, want cost %d µUSD, 3000 cache writes, none unpriced", model, row.Counters, want)
		}
	}
	if row := findRow(t, snap, "claude", "claude-opus-5-5", false); row.CostUSDMicros != 33_000 || row.UnpricedRequests != 1 {
		t.Errorf("opus: %+v, want 33000 µUSD and the empty request unpriced", row.Counters)
	}
	if row := findRow(t, snap, "codex", "gpt-5", false); row.CostUSDMicros != 0 || row.UnpricedRequests != 1 {
		t.Errorf("unknown model: %+v, want unpriced", row.Counters)
	}
}

func TestStateFileRoundTripsVersion2(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	clock := newClock()
	before := New(clock.Now, 0)
	before.Open(path)
	before.HandleUsage(context.Background(), claudeRecord("claude-opus-5-5", "a", 1000, 20_000, 3000, 500))
	if err := before.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var head struct{ Version int }
	if err := json.Unmarshal(data, &head); err != nil || head.Version != 2 {
		t.Fatalf("version %d (%v), want 2", head.Version, err)
	}
	after := New(clock.Now, 0)
	after.Open(path)
	for _, r := range allRanges {
		if got, want := after.Snapshot(r), before.Snapshot(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after restart:\n got %+v\nwant %+v", r, got, want)
		}
	}
	if row := findRow(t, after.Snapshot(RangeTotal), "claude", "claude-opus-5-5", false); row.CostUSDMicros != 33_000 {
		t.Fatalf("cost after restart: %+v", row.Counters)
	}
}

func TestVersion1StateBackfillsCostAsPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	clock := newClock()
	id := clock.Now().Unix() / BucketSeconds
	// v1 counters: input_tokens includes cached_tokens, which merges cache reads and writes.
	v1 := `{"version":1,"since":"2026-09-01T00:00:00Z","series":[
	 {"provider":"claude","model":"claude-opus-5-5","subagent":false,"last_id":ID,
	  "total":{"requests":3,"input_tokens":31000,"output_tokens":1500,"cached_tokens":20000,"total_tokens":32500},
	  "buckets":[{"id":ID,"requests":2,"input_tokens":21000,"output_tokens":500,"cached_tokens":20000,"total_tokens":21500}]},
	 {"provider":"codex","model":"gpt-5","subagent":false,"last_id":ID,
	  "total":{"requests":4,"input_tokens":10,"total_tokens":10},
	  "buckets":[{"id":ID,"requests":4,"input_tokens":10,"total_tokens":10}]}]}`
	v1 = strings.ReplaceAll(v1, "ID", strconv.FormatInt(id, 10))
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	agg := New(clock.Now, 0)
	agg.Open(path)

	// Bucket: 1,000 uncached x $4 + 20,000 cached as reads x $0.20 + 500 x $20.
	opus := findRow(t, agg.Snapshot(Range7d), "claude", "claude-opus-5-5", false)
	if opus.CostUSDMicros != 18_000 || !opus.CostPartial || opus.UnpricedRequests != 0 {
		t.Fatalf("7d opus: %+v, want 18000 µUSD partial", opus.Counters)
	}
	// Total adds the pruned history: 10,000 x $4 + 1,000 x $20.
	opus = findRow(t, agg.Snapshot(RangeTotal), "claude", "claude-opus-5-5", false)
	if opus.CostUSDMicros != 78_000 || !opus.CostPartial {
		t.Fatalf("total opus: %+v, want 78000 µUSD partial", opus.Counters)
	}
	if codex := findRow(t, agg.Snapshot(RangeTotal), "codex", "gpt-5", false); codex.UnpricedRequests != 4 || codex.CostUSDMicros != 0 {
		t.Fatalf("unknown model history: %+v, want all unpriced", codex.Counters)
	}

	agg.HandleUsage(context.Background(), claudeRecord("claude-opus-5-5", "a", 1000, 20_000, 3000, 500))
	clock.Advance(8 * 24 * time.Hour)
	agg.HandleUsage(context.Background(), claudeRecord("claude-opus-5-5", "a", 1000, 20_000, 3000, 500))
	if row := findRow(t, agg.Snapshot(RangeWindow), "claude", "claude-opus-5-5", false); row.CostPartial || row.CostUSDMicros != 33_000 {
		t.Fatalf("range after backfilled history: %+v, want exact", row.Counters)
	}
}
