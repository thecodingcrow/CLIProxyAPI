package modelusage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/pricing"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// FileName is the JSON state file, placed in WRITABLE_PATH or next to the config file.
	// No .json extension: the config dir may be the auth dir, where *.json files load as credentials.
	FileName = "model-usage.state"
	// FlushInterval is how often a dirty aggregator is written while persistence runs.
	FlushInterval = 60 * time.Second
	fileVersion   = 2
	// fileVersionUnpriced predates cost counters; its counts load backfilled at list price.
	fileVersionUnpriced = 1
)

type fileState struct {
	Version int          `json:"version"`
	Since   time.Time    `json:"since"`
	Series  []fileSeries `json:"series"`
}

type fileSeries struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Subagent bool     `json:"subagent"`
	LastID   int64    `json:"last_id"`
	Total    Counters `json:"total"`
	Buckets  []bucket `json:"buckets"`
}

// StatePath resolves the state file: WRITABLE_PATH when set, else the config file's directory, else "" (no persistence).
func StatePath(configPath string) string {
	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, FileName)
	}
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), FileName)
}

// Open merges the state file at path into a and makes it the Flush target.
// A missing file means empty state; an unreadable or corrupt one is renamed to .bak and ignored.
func (a *Aggregator) Open(path string) {
	a.mu.Lock()
	a.path = path
	a.mu.Unlock()

	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, fs.ErrNotExist) {
		return
	}
	var state fileState
	errParse := errRead
	if errParse == nil {
		errParse = json.Unmarshal(data, &state)
	}
	if errParse == nil && state.Version != fileVersion && state.Version != fileVersionUnpriced {
		errParse = fmt.Errorf("unsupported version %d", state.Version)
	}
	if errParse != nil {
		log.WithError(errParse).Warnf("model usage: ignoring unreadable state %s, starting empty", path)
		if errRename := os.Rename(path, path+".bak"); errRename != nil {
			log.WithError(errRename).Warnf("model usage: failed to keep %s as .bak", path)
		}
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	currentID := coreauth.RecentRequestBucketID(a.now())
	for _, fsr := range state.Series {
		if state.Version == fileVersionUnpriced {
			backfill(&fsr, state.Since)
		}
		s := a.seriesLocked(Key{Provider: fsr.Provider, Model: fsr.Model, Subagent: fsr.Subagent})
		// IDs ahead of the clock (file written by a clock running ahead) count in the current bucket.
		for _, b := range fsr.Buckets {
			s.add(min(b.ID, currentID), b.Counters, currentID)
		}
		s.total.add(fsr.Total)
		s.lastID = max(s.lastID, min(fsr.LastID, currentID))
	}
	if !state.Since.IsZero() && (a.since.IsZero() || state.Since.Before(a.since)) {
		a.since = state.Since
	}
}

// Flush writes the state atomically (temp file + rename) when it changed since the last flush.
func (a *Aggregator) Flush() error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()

	a.mu.Lock()
	if a.path == "" || !a.dirty {
		a.mu.Unlock()
		return nil
	}
	state := fileState{Version: fileVersion, Since: a.since, Series: make([]fileSeries, 0, len(a.series))}
	currentID := coreauth.RecentRequestBucketID(a.now())
	for key, s := range a.series {
		s.prune(currentID) // idle keys get no write that would prune them
		state.Series = append(state.Series, fileSeries{
			Provider: key.Provider, Model: key.Model, Subagent: key.Subagent,
			LastID: s.lastID, Total: s.total, Buckets: s.buckets,
		})
	}
	data, errMarshal := json.Marshal(state)
	path := a.path
	a.dirty = false
	a.mu.Unlock()

	errWrite := errMarshal
	if errWrite == nil {
		errWrite = writeAtomic(path, data)
	}
	if errWrite != nil {
		a.mu.Lock()
		a.dirty = true
		a.mu.Unlock()
		return fmt.Errorf("model usage: flush %s: %w", path, errWrite)
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, errWrite := tmp.Write(data)
	if errWrite == nil {
		errWrite = tmp.Sync()
	}
	if errClose := tmp.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Rename(tmpName, path)
	}
	if errWrite != nil {
		if errRemove := os.Remove(tmpName); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
			log.WithError(errRemove).Warnf("model usage: failed to remove %s", tmpName)
		}
	}
	return errWrite
}

// StartPersistence opens path and flushes every FlushInterval until StopPersistence; no-op for "" or when running.
func (a *Aggregator) StartPersistence(path string) {
	if path == "" {
		return
	}
	a.loopMu.Lock()
	defer a.loopMu.Unlock()
	if a.stop != nil {
		return
	}
	a.Open(path)
	log.Infof("model usage: persisting to %s", path)
	stop, done := make(chan struct{}), make(chan struct{})
	a.stop, a.done = stop, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(FlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := a.Flush(); err != nil {
					log.WithError(err).Warn("model usage: periodic flush failed")
				}
			}
		}
	}()
}

// StopPersistence ends the flush loop and writes a final flush.
func (a *Aggregator) StopPersistence() {
	a.loopMu.Lock()
	defer a.loopMu.Unlock()
	if a.stop == nil {
		return
	}
	close(a.stop)
	<-a.done
	a.stop, a.done = nil, nil
	if err := a.Flush(); err != nil {
		log.WithError(err).Warn("model usage: final flush failed")
	}
}

// backfill prices v1 counters with the price valid on each bucket's day. v1 merged cache reads and writes
// into cached_tokens, so all of it is priced as reads, and per-request long-context tiers are lost: partial.
// Total history older than the buckets has no date and is priced at the tracking start.
func backfill(fsr *fileSeries, since time.Time) {
	retained := Counters{}
	for i := range fsr.Buckets {
		b := &fsr.Buckets[i].Counters
		retained.add(*b)
		priceUnpriced(b, fsr.Model, time.Unix(fsr.Buckets[i].ID*BucketSeconds, 0))
	}
	older := fsr.Total
	older.Requests -= retained.Requests
	older.InputTokens -= retained.InputTokens
	older.CachedTokens -= retained.CachedTokens
	older.OutputTokens -= retained.OutputTokens
	priceUnpriced(&older, fsr.Model, since)
	priceUnpriced(&fsr.Total, fsr.Model, since) // flags the total and counts its unpriced requests
	fsr.Total.CostUSDMicros = older.CostUSDMicros
	for _, b := range fsr.Buckets {
		fsr.Total.CostUSDMicros += b.CostUSDMicros
	}
}

func priceUnpriced(c *Counters, model string, at time.Time) {
	if c.Requests <= 0 {
		return
	}
	c.CostPartial = true
	cost, ok := pricing.Default.Cost(model, at, pricing.Usage{
		Input:     max(0, c.InputTokens-c.CachedTokens),
		CacheRead: max(0, c.CachedTokens),
		Output:    max(0, c.OutputTokens),
	})
	if !ok {
		c.UnpricedRequests = c.Requests
		return
	}
	c.CostUSDMicros = cost
}
