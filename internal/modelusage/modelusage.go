// Package modelusage keeps per-provider/per-model token counters for the management dashboard:
// 10-minute buckets for the last 7 days plus an all-time total per key, optionally persisted to a file.
package modelusage

import (
	"context"
	"fmt"
	"os"
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
	// RetentionBuckets keeps 7 days of buckets per key; older ones survive only in the all-time total.
	RetentionBuckets = 7 * 24 * 3600 / BucketSeconds
	// DefaultMaxKeys bounds memory; past it the least recently active key is folded into the OtherName row.
	DefaultMaxKeys = 512
	// OtherName is provider and model of the row holding evicted keys, so no range loses counts on eviction.
	OtherName   = "other"
	unknownName = "unknown"
	// Executor identifier prefix for openai-compatibility entries.
	openAICompatibleProviderPrefix = "openai-compatible-"
)

// Range selects the span a snapshot sums over.
type Range string

const (
	RangeWindow Range = "window"
	RangeToday  Range = "today"
	Range7d     Range = "7d"
	RangeTotal  Range = "total"
)

// ParseRange maps the query value to a Range; empty means RangeWindow.
func ParseRange(value string) (Range, error) {
	switch r := Range(value); r {
	case "":
		return RangeWindow, nil
	case RangeWindow, RangeToday, Range7d, RangeTotal:
		return r, nil
	}
	return "", fmt.Errorf("invalid range %q: want window, today, 7d or total", value)
}

var otherKey = Key{Provider: OtherName, Model: OtherName}

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

// Row is one (provider, model, subagent) entry summed over the snapshot range.
type Row struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Subagent bool   `json:"subagent"`
	Counters
}

// Snapshot is the management endpoint payload.
type Snapshot struct {
	Range         Range `json:"range"`
	BucketSeconds int64 `json:"bucket_seconds"`
	// WindowMinutes is the covered span: since to now.
	WindowMinutes int64  `json:"window_minutes"`
	Since         string `json:"since"`
	Timezone      string `json:"timezone"`
	GeneratedAt   string `json:"generated_at"`
	Models        []Row  `json:"models"`
}

type bucket struct {
	ID int64 `json:"id"`
	Counters
}

type series struct {
	buckets []bucket // ascending ID, none older than RetentionBuckets
	total   Counters
	lastID  int64
}

// add counts c in bucket id and drops buckets that fell out of retention relative to id.
func (s *series) add(id int64, c Counters) {
	n := len(s.buckets)
	switch {
	case n > 0 && s.buckets[n-1].ID == id:
		s.buckets[n-1].add(c)
	case n == 0 || s.buckets[n-1].ID < id:
		s.buckets = append(s.buckets, bucket{ID: id, Counters: c})
		s.prune(id)
	default: // clock stepped back
		i := sort.Search(n, func(i int) bool { return s.buckets[i].ID >= id })
		if s.buckets[i].ID == id {
			s.buckets[i].add(c)
		} else {
			s.buckets = append(s.buckets[:i], append([]bucket{{ID: id, Counters: c}}, s.buckets[i:]...)...)
		}
	}
	if id > s.lastID {
		s.lastID = id
	}
}

func (s *series) prune(currentID int64) {
	i := sort.Search(len(s.buckets), func(i int) bool { return s.buckets[i].ID > currentID-RetentionBuckets })
	s.buckets = s.buckets[i:]
}

// Aggregator is a usage plugin holding one bucket series per Key.
type Aggregator struct {
	mu      sync.Mutex
	now     func() time.Time
	maxKeys int
	series  map[Key]*series
	since   time.Time // first record ever counted, the start of RangeTotal
	dirty   bool
	path    string

	flushMu sync.Mutex
	loopMu  sync.Mutex
	stop    chan struct{}
	done    chan struct{}
}

// New builds an aggregator; now is the bucket clock and its location defines RangeToday, maxKeys <= 0 means DefaultMaxKeys.
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
	now := a.now()
	s := a.seriesLocked(key)
	s.add(coreauth.RecentRequestBucketID(now), counters)
	s.total.add(counters)
	if a.since.IsZero() {
		a.since = now
	}
	a.dirty = true
}

// seriesLocked returns key's series, creating it after making room under the key cap.
func (a *Aggregator) seriesLocked(key Key) *series {
	if s, ok := a.series[key]; ok {
		return s
	}
	if key != otherKey {
		a.makeRoomLocked()
	}
	s := &series{}
	a.series[key] = s
	return s
}

// makeRoomLocked folds the least recently active keys into otherKey while at the cap; otherKey is outside it.
func (a *Aggregator) makeRoomLocked() {
	for {
		capped := len(a.series)
		if _, ok := a.series[otherKey]; ok {
			capped--
		}
		if capped < a.maxKeys {
			return
		}
		var oldest Key
		var victim *series
		for key, s := range a.series {
			if key != otherKey && (victim == nil || s.lastID < victim.lastID) {
				oldest, victim = key, s
			}
		}
		delete(a.series, oldest)
		other := a.seriesLocked(otherKey)
		for _, b := range victim.buckets {
			other.add(b.ID, b.Counters)
		}
		other.total.add(victim.total)
	}
}

// Snapshot sums each key over r; zero-request rows are omitted, order is total_tokens desc.
func (a *Aggregator) Snapshot(r Range) Snapshot {
	if r == "" {
		r = RangeWindow
	}
	out := Snapshot{Range: r, BucketSeconds: BucketSeconds, Models: []Row{}}
	if a == nil {
		a = New(nil, 0)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out.GeneratedAt = now.UTC().Format(time.RFC3339)
	out.Timezone = zoneName(now.Location())
	currentID := coreauth.RecentRequestBucketID(now)

	var fromID int64 // first bucket counted; RangeTotal reads the all-time totals instead
	var since time.Time
	switch r {
	case RangeToday:
		since = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		fromID = (since.Unix() + BucketSeconds - 1) / BucketSeconds // bucket counts when it starts at or after midnight
	case Range7d:
		since = now.Add(-time.Duration(RetentionBuckets*BucketSeconds) * time.Second)
		fromID = currentID - RetentionBuckets + 1
	case RangeTotal:
		since = a.since
		if since.IsZero() {
			since = now
		}
	default:
		since = now.Add(-time.Duration(WindowMinutes) * time.Minute)
		fromID = currentID - bucketCount + 1
	}
	out.Since = since.In(now.Location()).Format(time.RFC3339)
	out.WindowMinutes = int64(now.Sub(since).Round(time.Minute) / time.Minute)

	for key, s := range a.series {
		row := Row{Provider: key.Provider, Model: key.Model, Subagent: key.Subagent}
		if r == RangeTotal {
			row.Counters = s.total
		} else {
			for _, b := range s.buckets {
				if b.ID >= fromID && b.ID <= currentID {
					row.add(b.Counters)
				}
			}
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

// zoneName returns the IANA name of loc; time.Local only knows itself as "Local".
func zoneName(loc *time.Location) string {
	name := loc.String()
	if name != "Local" {
		return name
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, after, ok := strings.Cut(target, "zoneinfo/"); ok {
			return after
		}
	}
	return name
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
