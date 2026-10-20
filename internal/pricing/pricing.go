// Package pricing holds Anthropic's dated API list prices and the API-equivalent cost of one request.
package pricing

import (
	"regexp"
	"strings"
	"time"
)

// Prices are micro-USD per million tokens, so a whole-dollar price is exact and a token costs Prices/1e6 µUSD.
type Prices struct {
	Input        int64
	CacheWrite5m int64
	CacheWrite1h int64
	CacheRead    int64
	Output       int64
}

// Tier replaces every price of a request whose prompt exceeds OverPromptTokens.
type Tier struct {
	OverPromptTokens int64
	Prices
}

// Entry prices Model from From (zero: since ever) until a later entry for the same model.
type Entry struct {
	Model string
	From  time.Time
	Prices
	LongContext *Tier
}

// Usage is one request's tokens in non-overlapping classes; Input excludes cache reads and writes.
type Usage struct {
	Input        int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
	Output       int64
}

func (u Usage) prompt() int64 { return u.Input + u.CacheRead + u.CacheWrite5m + u.CacheWrite1h }

// Table is a list of dated entries; order does not matter.
type Table []Entry

func usd(dollars float64) int64 { return int64(dollars*1e6 + 0.5) }

// row builds an entry with the cache-write prices fixed at 1.25x (5m) and 2x (1h) of input.
func row(model string, input, read, output float64) Entry {
	return Entry{Model: model, Prices: Prices{
		Input: usd(input), CacheWrite5m: usd(input * 1.25), CacheWrite1h: usd(input * 2), CacheRead: usd(read), Output: usd(output),
	}}
}

// Default is https://platform.claude.com/docs/en/about-claude/pricing as of 2026-10-21.
// A price change is a new entry with a later From, never an edit: history keeps its price.
var Default = Table{
	row("claude-fable-5-1", 10, 0.25, 50),
	row("claude-mythos-5-1", 10, 0.25, 50),
	row("claude-fable-5", 10, 1, 50),
	row("claude-mythos-5", 10, 1, 50),
	row("claude-opus-5-5", 4, 0.20, 20),
	row("claude-opus-5", 5, 0.50, 25),
	row("claude-opus-4-8", 5, 0.50, 25),
	row("claude-opus-4-7", 5, 0.50, 25),
	row("claude-opus-4-6", 5, 0.50, 25),
	row("claude-opus-4-5", 5, 0.50, 25),
	row("claude-opus-4-1", 15, 1.50, 75),
	row("claude-opus-4", 15, 1.50, 75),
	row("claude-sonnet-5-5", 2, 0.10, 10),
	row("claude-sonnet-5", 2, 0.20, 10),
	row("claude-sonnet-4-6", 3, 0.30, 15),
	row("claude-sonnet-4-5", 3, 0.30, 15),
	row("claude-sonnet-4", 3, 0.30, 15),
	withTier(row("claude-haiku-5-5", 0.10, 0.01, 0.50), 100_000, row("", 0.50, 0.05, 2.50)),
	row("claude-haiku-4-5", 1, 0.10, 5),
	row("claude-haiku-3-5", 0.80, 0.08, 4),
}

func withTier(e Entry, over int64, tier Entry) Entry {
	e.LongContext = &Tier{OverPromptTokens: over, Prices: tier.Prices}
	return e
}

// Lookup returns the entry for model valid at at.
func (t Table) Lookup(model string, at time.Time) (Entry, bool) {
	id := Normalize(model)
	var best Entry
	found := false
	for _, e := range t {
		if e.Model == id && !e.From.After(at) && (!found || e.From.After(best.From)) {
			best, found = e, true
		}
	}
	return best, found
}

// Cost is u priced at the entry valid at at, in µUSD rounded once per request; false for an unknown model.
func (t Table) Cost(model string, at time.Time, u Usage) (int64, bool) {
	e, ok := t.Lookup(model, at)
	if !ok {
		return 0, false
	}
	if e.LongContext != nil && u.prompt() > e.LongContext.OverPromptTokens {
		return e.LongContext.Cost(u), true
	}
	return e.Prices.Cost(u), true
}

// Cost is u at p in µUSD, rounded once; no tier applies, so summed usage can be priced at base rates.
func (p Prices) Cost(u Usage) int64 {
	// tokens x µUSD/MTok is picoUSD: exact until the final rounding.
	pico := u.Input*p.Input + u.CacheRead*p.CacheRead + u.CacheWrite5m*p.CacheWrite5m + u.CacheWrite1h*p.CacheWrite1h + u.Output*p.Output
	return (pico + 500_000) / 1_000_000
}

var (
	// Suffixes a provider or the proxy appends to a model id: Bedrock version, Vertex or dated snapshot, alias tags.
	suffixRe = regexp.MustCompile(`(-v\d+(:\d+)?|@\d{8}|-\d{8}|-latest|-thinking|\[1m\])$`)
	// Pre-4.5 ids put the family after the version: claude-3-5-haiku.
	legacyRe = regexp.MustCompile(`^claude-(\d+)-(\d+)-(haiku|sonnet|opus)$`)
)

// Normalize maps a provider's model id onto the table's id.
func Normalize(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if base, _, ok := strings.Cut(id, "("); ok { // thinking budget suffix: model(16384)
		id = base
	}
	if i := strings.LastIndex(id, "anthropic."); i >= 0 {
		id = id[i+len("anthropic."):]
	}
	id = strings.TrimPrefix(id, "anthropic/")
	for {
		trimmed := suffixRe.ReplaceAllString(id, "")
		if trimmed == id {
			break
		}
		id = trimmed
	}
	if m := legacyRe.FindStringSubmatch(id); m != nil {
		id = "claude-" + m[3] + "-" + m[1] + "-" + m[2]
	}
	return strings.TrimSuffix(id, "-0")
}
