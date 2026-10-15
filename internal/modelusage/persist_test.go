package modelusage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var allRanges = []Range{RangeWindow, RangeToday, Range7d, RangeTotal}

func TestRestartRestoresEveryRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	clock := newClock()
	start := clock.Now()
	before := New(clock.Now, 0)
	before.Open(path)
	before.Add(Key{Provider: "p", Model: "old"}, Counters{Requests: 1, TotalTokens: 100})
	clock.Advance(8 * 24 * time.Hour) // "old" now lives only in total
	before.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 2, Failed: 1, InputTokens: 3, TotalTokens: 3})
	before.Add(Key{Provider: "p", Model: "m", Subagent: true}, Counters{Requests: 1, OutputTokens: 4, TotalTokens: 4})
	if err := before.Flush(); err != nil {
		t.Fatal(err)
	}

	after := New(clock.Now, 0)
	after.Open(path)
	for _, r := range allRanges {
		if got, want := after.Snapshot(r), before.Snapshot(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s after restart:\n got %+v\nwant %+v", r, got, want)
		}
	}

	clock.Advance(time.Hour)
	after.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1, TotalTokens: 10})
	total := after.Snapshot(RangeTotal)
	if got := findRow(t, total, "p", "m", false); got.Requests != 3 || got.TotalTokens != 13 {
		t.Fatalf("total keeps accumulating: %+v", got)
	}
	if want := start.Format(time.RFC3339); total.Since != want {
		t.Fatalf("total since %s, want tracking start %s", total.Since, want)
	}
}

func TestCorruptFileStartsEmptyAndKeepsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := newClock()
	agg := New(clock.Now, 0)
	agg.Open(path)
	if rows := agg.Snapshot(RangeTotal).Models; len(rows) != 0 {
		t.Fatalf("want empty state, got %+v", rows)
	}
	if bak, err := os.ReadFile(path + ".bak"); err != nil || string(bak) != "{not json" {
		t.Fatalf("backup: %q, %v", bak, err)
	}

	agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1})
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	reloaded := New(clock.Now, 0)
	reloaded.Open(path)
	findRow(t, reloaded.Snapshot(RangeTotal), "p", "m", false)
}

func TestMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", FileName)
	agg := New(newClock().Now, 0)
	agg.Open(path)
	if err := agg.Flush(); err != nil {
		t.Fatalf("clean flush: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("nothing recorded, nothing written: %v", err)
	}
	agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1})
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("flush creates missing directory and file: %v", err)
	}
}

// oldestBucketIDs returns, per key, the oldest bucket ID held in memory and in the state file.
func oldestBucketIDs(t *testing.T, agg *Aggregator, path string) (memory, file map[string]int64) {
	t.Helper()
	memory, file = map[string]int64{}, map[string]int64{}
	agg.mu.Lock()
	for key, s := range agg.series {
		if len(s.buckets) > 0 {
			memory[key.Model] = s.buckets[0].ID
		}
	}
	agg.mu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state fileState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	for _, s := range state.Series {
		for _, b := range s.Buckets {
			if old, ok := file[s.Model]; !ok || b.ID < old {
				file[s.Model] = b.ID
			}
		}
	}
	return memory, file
}

func TestPruneKeepsOnlySevenDaysInMemoryAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	clock := newClock()
	agg := New(clock.Now, 0)
	agg.Open(path)
	agg.Add(Key{Provider: "p", Model: "busy"}, Counters{Requests: 1})
	agg.Add(Key{Provider: "p", Model: "idle"}, Counters{Requests: 1})
	clock.Advance(8 * 24 * time.Hour)
	agg.Add(Key{Provider: "p", Model: "busy"}, Counters{Requests: 1})
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	firstKept := coreauth.RecentRequestBucketID(clock.Now()) - RetentionBuckets + 1

	memory, file := oldestBucketIDs(t, agg, path)
	for _, view := range []map[string]int64{memory, file} {
		for model, oldest := range view {
			if oldest < firstKept {
				t.Fatalf("%s keeps bucket %d older than 7d (first kept %d); memory %v file %v", model, oldest, firstKept, memory, file)
			}
		}
	}
	if file["busy"] == 0 {
		t.Fatalf("busy key's current bucket missing from file: %v", file)
	}

	reloaded := New(clock.Now, 0)
	reloaded.Open(path)
	reloaded.mu.Lock()
	for key, s := range reloaded.series {
		if len(s.buckets) > 0 && s.buckets[0].ID < firstKept {
			t.Errorf("reloaded %s keeps bucket %d older than 7d (first kept %d)", key.Model, s.buckets[0].ID, firstKept)
		}
	}
	reloaded.mu.Unlock()
	if got := findRow(t, reloaded.Snapshot(RangeTotal), "p", "idle", false); got.Requests != 1 {
		t.Fatalf("idle total after prune: %+v", got)
	}
}

// A file written by a clock running ahead must neither hide its counts nor stop pruning.
func TestFutureBucketsFromFileAreClampedAndPruningContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	clock := newClock()
	ahead := &fakeClock{now: clock.Now().Add(8 * 24 * time.Hour)}
	writer := New(ahead.Now, 0)
	writer.Open(path)
	writer.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1, TotalTokens: 4})
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}

	agg := New(clock.Now, 0)
	agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1, TotalTokens: 1}) // within 7 days of now
	agg.Open(path)
	agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1, TotalTokens: 2})
	if got := sumTokens(agg.Snapshot(Range7d)); got != 7 {
		t.Fatalf("7d tokens %d, want 7: real buckets kept, future bucket counted now", got)
	}

	clock.Advance(8 * 24 * time.Hour)
	agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1, TotalTokens: 8})
	if got := sumTokens(agg.Snapshot(Range7d)); got != 8 {
		t.Fatalf("7d tokens %d after 8 days, want 8", got)
	}
	agg.mu.Lock()
	n := len(agg.series[Key{Provider: "p", Model: "m"}].buckets)
	agg.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d buckets held, want 1: pruning stalled behind a future bucket", n)
	}
}

func TestFailedFlushIsRetriedAndCleanFlushWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	agg := New(newClock().Now, 0)
	agg.Open(path)
	if err := os.Mkdir(path, 0o755); err != nil { // rename onto a directory fails
		t.Fatal(err)
	}
	agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1})
	if err := agg.Flush(); err == nil {
		t.Fatal("flush onto a directory succeeded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	reloaded := New(newClock().Now, 0)
	reloaded.Open(path)
	findRow(t, reloaded.Snapshot(RangeTotal), "p", "m", false)

	if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "sentinel" {
		t.Fatalf("flush without new usage rewrote the file: %q", raw)
	}
}

func TestStatePath(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	if got, want := StatePath("/etc/cpa/config.yaml"), filepath.Join("/etc/cpa", FileName); got != want {
		t.Fatalf("next to config: %s, want %s", got, want)
	}
	if got := StatePath(""); got != "" {
		t.Fatalf("no config, no persistence: %s", got)
	}
	t.Setenv("WRITABLE_PATH", "/var/lib/cpa")
	if got, want := StatePath("/etc/cpa/config.yaml"), filepath.Join("/var/lib/cpa", FileName); got != want {
		t.Fatalf("writable path wins: %s, want %s", got, want)
	}
}

func TestConcurrentAddSnapshotFlush(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 16)
	agg.Open(filepath.Join(t.TempDir(), FileName))
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				agg.Add(Key{Provider: "p", Model: fmt.Sprintf("m%d", (w+i)%24)}, Counters{Requests: 1, TotalTokens: 1})
				if i%50 == 0 {
					clock.Advance(time.Hour)
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = agg.Snapshot(allRanges[(r+i)%len(allRanges)])
			}
		}(r)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := agg.Flush(); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()

	var requests int64
	for _, row := range agg.Snapshot(RangeTotal).Models {
		requests += row.Requests
	}
	if requests != 8*200 {
		t.Fatalf("total requests %d, want %d: eviction must not lose counts", requests, 8*200)
	}
}
