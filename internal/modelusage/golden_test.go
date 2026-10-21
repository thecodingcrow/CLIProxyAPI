package modelusage

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// The wire contract of GET /v8/management/observability/usage/models, field names verbatim.
func TestSnapshotJSONMatchesContract(t *testing.T) {
	agg, _, pool := quotaFixture(t, true)
	pool.Stale = true
	checkGolden(t, agg.SnapshotWithQuota(Range7d, &pool), "models-quota.golden.json")
	rough, roughPool := roughFixture(t)
	checkGolden(t, rough.SnapshotWithQuota(Range7d, roughPool), "models-quota-rough.golden.json")
	carried, clock, resets := weekOne(t)
	rollTo(carried, resets)
	checkGolden(t, carried.SnapshotWithQuota(RangeTotal, soloPool(1, resets, clock.Now())), "models-quota-carried.golden.json")
	if partial, _ := json.Marshal(Counters{CostPartial: true}); !bytes.Contains(partial, []byte(`"cost_partial":true`)) {
		t.Fatalf("cost_partial field: %s", partial)
	}
}

func checkGolden(t *testing.T, snap Snapshot, name string) {
	t.Helper()
	got, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(got, '\n'), want) {
		t.Fatalf("snapshot JSON differs from %s (rerun with -update to inspect):\n%s", path, got)
	}
}
