package modelusage

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
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
