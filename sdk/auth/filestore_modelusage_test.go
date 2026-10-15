package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelusage"
)

// The model usage state file sits next to the config, which may also be the auth dir.
func TestFileTokenStoreIgnoresModelUsageStateFile(t *testing.T) {
	dir := t.TempDir()
	agg := modelusage.New(time.Now, 0)
	agg.Open(filepath.Join(dir, modelusage.FileName))
	agg.Add(modelusage.Key{Provider: "claude", Model: "m"}, modelusage.Counters{Requests: 1})
	if err := agg.Flush(); err != nil {
		t.Fatal(err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 0 {
		t.Fatalf("model usage state listed as %d credential(s), first %q", len(auths), auths[0].ID)
	}
}
