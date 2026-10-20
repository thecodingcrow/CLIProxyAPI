package pricing

import (
	"testing"
	"time"
)

const mtok = 1_000_000

var day = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// Expected USD per MTok, copied from platform.claude.com/docs/en/about-claude/pricing on 2026-10-21.
var official = []struct {
	model                                 string
	input, write5m, write1h, read, output float64
}{
	{"claude-fable-5-1", 10, 12.50, 20, 0.25, 50},
	{"claude-mythos-5-1", 10, 12.50, 20, 0.25, 50},
	{"claude-fable-5", 10, 12.50, 20, 1, 50},
	{"claude-mythos-5", 10, 12.50, 20, 1, 50},
	{"claude-opus-5-5", 4, 5, 8, 0.20, 20},
	{"claude-opus-5", 5, 6.25, 10, 0.50, 25},
	{"claude-opus-4-8", 5, 6.25, 10, 0.50, 25},
	{"claude-opus-4-7", 5, 6.25, 10, 0.50, 25},
	{"claude-opus-4-6", 5, 6.25, 10, 0.50, 25},
	{"claude-opus-4-5", 5, 6.25, 10, 0.50, 25},
	{"claude-opus-4-1", 15, 18.75, 30, 1.50, 75},
	{"claude-opus-4", 15, 18.75, 30, 1.50, 75},
	{"claude-sonnet-5-5", 2, 2.50, 4, 0.10, 10},
	{"claude-sonnet-5", 2, 2.50, 4, 0.20, 10},
	{"claude-sonnet-4-6", 3, 3.75, 6, 0.30, 15},
	{"claude-sonnet-4-5", 3, 3.75, 6, 0.30, 15},
	{"claude-sonnet-4", 3, 3.75, 6, 0.30, 15},
	{"claude-haiku-5-5", 0.10, 0.125, 0.20, 0.01, 0.50},
	{"claude-haiku-4-5", 1, 1.25, 2, 0.10, 5},
	{"claude-haiku-3-5", 0.80, 1, 1.60, 0.08, 4},
}

func micros(usd float64) int64 { return int64(usd*1e6 + 0.5) }

func mustCost(t *testing.T, table Table, model string, at time.Time, u Usage) int64 {
	t.Helper()
	got, ok := table.Cost(model, at, u)
	if !ok {
		t.Fatalf("%s: not priced", model)
	}
	return got
}

func TestEveryOfficialModelPricesEachTokenClass(t *testing.T) {
	// 100,000 tokens: largest prompt still on Haiku 5.5 base rates.
	const tenth = mtok / 10
	for _, m := range official {
		for _, c := range []struct {
			name string
			u    Usage
			want float64
		}{
			{"input", Usage{Input: tenth}, m.input},
			{"5m write", Usage{CacheWrite5m: tenth}, m.write5m},
			{"1h write", Usage{CacheWrite1h: tenth}, m.write1h},
			{"cache read", Usage{CacheRead: tenth}, m.read},
			{"output", Usage{Output: tenth}, m.output},
		} {
			if got, want := mustCost(t, Default, m.model, day, c.u), micros(c.want/10); got != want {
				t.Errorf("%s %s: %d micros, want %d", m.model, c.name, got, want)
			}
		}
	}
}

func TestCacheReadMultiplier(t *testing.T) {
	for model, ratio := range map[string]float64{
		"claude-fable-5-1": 0.025, "claude-mythos-5-1": 0.025,
		"claude-opus-5-5": 0.05, "claude-sonnet-5-5": 0.05,
		"claude-opus-4-5": 0.1, "claude-haiku-4-5": 0.1, "claude-sonnet-5": 0.1, "claude-haiku-5-5": 0.1,
	} {
		read := mustCost(t, Default, model, day, Usage{CacheRead: 10_000})
		input := mustCost(t, Default, model, day, Usage{Input: 10_000})
		if got := float64(read) / float64(input); got < ratio-1e-9 || got > ratio+1e-9 {
			t.Errorf("%s cache read %.4fx input, want %.3fx", model, got, ratio)
		}
	}
}

func TestHaiku55LongContextTierStartsAbove100kPromptTokens(t *testing.T) {
	// Prompt = uncached input + cache reads + cache writes.
	at := Usage{Input: 40_000, CacheRead: 50_000, CacheWrite5m: 10_000, Output: 1_000}
	over := at
	over.Input++
	// 40k*0.10 + 50k*0.01 + 10k*0.125 + 1k*0.50 = 4000+500+1250+500 µUSD
	if got := mustCost(t, Default, "claude-haiku-5-5", day, at); got != 6250 {
		t.Fatalf("100,000 prompt tokens: %d micros, want 6250 (base rates)", got)
	}
	// 40,001*0.50 + 50k*0.05 + 10k*0.625 + 1k*2.50 = 20000.5+2500+6250+2500 µUSD
	if got := mustCost(t, Default, "claude-haiku-5-5", day, over); got != 31251 {
		t.Fatalf("100,001 prompt tokens: %d micros, want 31251 (long-context rates)", got)
	}
}

func TestLaterEntryAppliesFromItsDate(t *testing.T) {
	from := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	table := append(Table{}, Default...)
	table = append(table, Entry{Model: "claude-opus-5-5", From: from, Prices: Prices{Input: usd(8), CacheWrite5m: usd(10), CacheWrite1h: usd(16), CacheRead: usd(0.4), Output: usd(40)}})
	u := Usage{Input: mtok}
	if got := mustCost(t, table, "claude-opus-5-5", from.Add(-time.Nanosecond), u); got != 4_000_000 {
		t.Fatalf("before from: %d, want old price", got)
	}
	if got := mustCost(t, table, "claude-opus-5-5", from, u); got != 8_000_000 {
		t.Fatalf("at from: %d, want new price", got)
	}
	if got := mustCost(t, table, "claude-sonnet-5-5", from, u); got != 2_000_000 {
		t.Fatalf("other model unchanged: %d", got)
	}
}

func TestModelIDsNormalise(t *testing.T) {
	for _, id := range []string{
		"claude-opus-5-5", "Claude-Opus-5-5", " claude-opus-5-5 ", "claude-opus-5-5-20261001", "claude-opus-5-5[1m]",
		"claude-opus-5-5(16384)", "claude-opus-5-5-thinking", "claude-opus-5-5-latest", "anthropic/claude-opus-5-5",
		"us.anthropic.claude-opus-5-5-20261001-v1:0", "claude-opus-5-5@20261001",
	} {
		if got := mustCost(t, Default, id, day, Usage{Input: mtok}); got != 4_000_000 {
			t.Errorf("%q: %d, want Opus 5.5 input price", id, got)
		}
	}
	for id, want := range map[string]int64{
		"claude-3-5-haiku-20241022": 800_000, "claude-opus-4-0": 15_000_000, "claude-sonnet-4-0": 3_000_000,
		"claude-opus-4-20250514": 15_000_000, "claude-haiku-4-5-20251001": 1_000_000,
	} {
		if got := mustCost(t, Default, id, day, Usage{Input: mtok}); got != want {
			t.Errorf("%q: %d, want %d", id, got, want)
		}
	}
}

func TestUnknownModelIsNotPriced(t *testing.T) {
	for _, id := range []string{"", "gpt-5", "claude-3-7-sonnet-latest", "claude-opus-5-55", "claude-fable"} {
		if got, ok := Default.Cost(id, day, Usage{Input: mtok}); ok {
			t.Errorf("%q priced at %d", id, got)
		}
	}
}
