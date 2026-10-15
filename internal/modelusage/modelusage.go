// Package modelusage keeps windowed per-provider/per-model token counters for the management dashboard.
package modelusage

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

const (
	// BucketSeconds and bucketCount mirror the per-auth recent-request ring so both views cover one window.
	BucketSeconds = coreauth.RecentRequestBucketSeconds
	bucketCount   = coreauth.RecentRequestBucketCount
	// WindowMinutes is the live window covered by a snapshot.
	WindowMinutes = BucketSeconds * bucketCount / 60
	// DefaultMaxKeys bounds memory; past it the least recently active key is evicted.
	DefaultMaxKeys = 512
	unknownName    = "unknown"
	// Executor identifier prefix for openai-compatibility entries.
	openAICompatibleProviderPrefix = "openai-compatible-"
)

// Key identifies one aggregated series.
type Key struct {
	Provider string
	Model    string
	Subagent bool
}

// Counters are summed per bucket and per snapshot row.
// Input includes cached tokens, output includes reasoning tokens, total = input + output.
type Counters struct {
	Requests        int64 `json:"requests"`
	Failed          int64 `json:"failed"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

func (c *Counters) add(o Counters) {
	c.Requests += o.Requests
	c.Failed += o.Failed
	c.InputTokens += o.InputTokens
	c.OutputTokens += o.OutputTokens
	c.CachedTokens += o.CachedTokens
	c.ReasoningTokens += o.ReasoningTokens
	c.TotalTokens += o.TotalTokens
}

// Row is one (provider, model, subagent) entry summed over the live window.
type Row struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Subagent bool   `json:"subagent"`
	Counters
}

// Snapshot is the management endpoint payload.
type Snapshot struct {
	BucketSeconds int64  `json:"bucket_seconds"`
	WindowMinutes int64  `json:"window_minutes"`
	GeneratedAt   string `json:"generated_at"`
	Models        []Row  `json:"models"`
}

type bucket struct {
	id int64
	Counters
}

type series struct {
	buckets [bucketCount]bucket
	lastID  int64
}

// Aggregator is a usage plugin holding one bucket ring per Key.
type Aggregator struct {
	mu      sync.Mutex
	now     func() time.Time
	maxKeys int
	series  map[Key]*series
}

// New builds an aggregator; now is the bucket clock, maxKeys <= 0 means DefaultMaxKeys.
func New(now func() time.Time, maxKeys int) *Aggregator {
	if now == nil {
		now = time.Now
	}
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	return &Aggregator{now: now, maxKeys: maxKeys, series: make(map[Key]*series)}
}

var defaultAggregator = New(time.Now, DefaultMaxKeys)

// Always on: independent of the usage-statistics toggle that gates the Redis queue.
func init() { coreusage.RegisterPlugin(defaultAggregator) }

// Default returns the process-wide aggregator fed by the usage manager.
func Default() *Aggregator { return defaultAggregator }

func bucketIndex(id int64) int {
	mod := id % bucketCount
	if mod < 0 {
		mod += bucketCount
	}
	return int(mod)
}

// HandleUsage implements coreusage.Plugin.
func (a *Aggregator) HandleUsage(ctx context.Context, record coreusage.Record) {
	if a == nil {
		return
	}
	key, counters := fromRecord(ctx, record)
	a.Add(key, counters)
}

// Add records counters for key in the bucket of the current clock time.
func (a *Aggregator) Add(key Key, counters Counters) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	id := coreauth.RecentRequestBucketID(a.now())
	s, ok := a.series[key]
	if !ok {
		a.makeRoomLocked(id)
		s = &series{}
		a.series[key] = s
	}
	b := &s.buckets[bucketIndex(id)]
	if b.id != id {
		*b = bucket{id: id}
	}
	b.add(counters)
	if id > s.lastID {
		s.lastID = id
	}
}

// makeRoomLocked drops expired series, then the least recently active one while at the cap.
func (a *Aggregator) makeRoomLocked(currentID int64) {
	if len(a.series) < a.maxKeys {
		return
	}
	a.dropExpiredLocked(currentID)
	for len(a.series) >= a.maxKeys {
		var oldest Key
		oldestID := int64(-1)
		for key, s := range a.series {
			if oldestID < 0 || s.lastID < oldestID {
				oldest, oldestID = key, s.lastID
			}
		}
		delete(a.series, oldest)
	}
}

func (a *Aggregator) dropExpiredLocked(currentID int64) {
	for key, s := range a.series {
		if s.lastID <= currentID-bucketCount {
			delete(a.series, key)
		}
	}
}

// Snapshot sums each key over the live window; zero-request rows are omitted, order is total_tokens desc.
func (a *Aggregator) Snapshot() Snapshot {
	out := Snapshot{BucketSeconds: BucketSeconds, WindowMinutes: WindowMinutes, Models: []Row{}}
	if a == nil {
		out.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out.GeneratedAt = now.UTC().Format(time.RFC3339)
	currentID := coreauth.RecentRequestBucketID(now)
	a.dropExpiredLocked(currentID)
	for key, s := range a.series {
		row := Row{Provider: key.Provider, Model: key.Model, Subagent: key.Subagent}
		for i := range s.buckets {
			b := &s.buckets[i]
			if b.id <= currentID-bucketCount || b.id > currentID {
				continue
			}
			row.add(b.Counters)
		}
		if row.Requests == 0 {
			continue
		}
		out.Models = append(out.Models, row)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		x, y := out.Models[i], out.Models[j]
		if x.TotalTokens != y.TotalTokens {
			return x.TotalTokens > y.TotalTokens
		}
		if x.Requests != y.Requests {
			return x.Requests > y.Requests
		}
		if x.Provider != y.Provider {
			return x.Provider < y.Provider
		}
		if x.Model != y.Model {
			return x.Model < y.Model
		}
		return !x.Subagent && y.Subagent
	})
	return out
}

// fromRecord resolves session and failure the same way the Redis usage queue does.
func fromRecord(ctx context.Context, record coreusage.Record) (Key, Counters) {
	provider := strings.ToLower(strings.TrimSpace(record.Provider))
	// Compat executors report "openai-compatible-<name>"; the api-keys view groups them by compat name.
	if compatName := strings.TrimPrefix(provider, openAICompatibleProviderPrefix); compatName != provider && compatName != "" {
		provider = compatName
	}
	if provider == "" {
		provider = unknownName
	}
	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = unknownName
	}

	clientRequestMetadata := internallogging.GetClientRequestMetadata(ctx)
	sessionID := strings.TrimSpace(record.SessionID)
	parentSessionID := strings.TrimSpace(record.ParentSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(clientRequestMetadata.SessionID)
		parentSessionID = strings.TrimSpace(clientRequestMetadata.ParentSessionID)
	} else if parentSessionID == "" && sessionID == strings.TrimSpace(clientRequestMetadata.SessionID) {
		parentSessionID = strings.TrimSpace(clientRequestMetadata.ParentSessionID)
	}
	sessionID = coresession.NormalizeToCanonicalUUID(sessionID)
	parentSessionID = coresession.NormalizeToCanonicalUUID(parentSessionID)
	if sessionID == "" || sessionID == parentSessionID {
		parentSessionID = ""
	}

	failed := record.Failed
	if !failed {
		status := internallogging.GetResponseStatus(ctx)
		failed = status >= 400
	}

	counters := tokenCounters(coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType))
	counters.Requests = 1
	if failed {
		counters.Failed = 1
	}
	return Key{Provider: provider, Model: model, Subagent: parentSessionID != ""}, counters
}

// tokenCounters prefers the normalised breakdown: raw Detail fields mean different things per provider.
func tokenCounters(detail coreusage.Detail) Counters {
	if bd := detail.TokenBreakdown; bd.Quality == coreusage.TokenAccountingQualityComplete {
		return Counters{
			InputTokens:     bd.Input.TotalTokens,
			OutputTokens:    bd.Output.TotalTokens,
			CachedTokens:    bd.Input.CacheReadTokens + bd.Input.CacheWriteTokens,
			ReasoningTokens: bd.Output.ReasoningTokens,
			TotalTokens:     bd.TotalTokens,
		}
	}
	return Counters{
		InputTokens:     detail.InputTokens,
		OutputTokens:    detail.OutputTokens,
		CachedTokens:    detail.CacheReadTokens + detail.CacheCreationTokens,
		ReasoningTokens: detail.ReasoningTokens,
		TotalTokens:     detail.TotalTokens,
	}
}
