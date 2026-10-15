package modelusage

import (
	"testing"
	"time"
)

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

func at(t *testing.T, loc *time.Location, value string) time.Time {
	t.Helper()
	ts, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func sumTokens(snap Snapshot) int64 {
	var total int64
	for _, row := range snap.Models {
		total += row.TotalTokens
	}
	return total
}

// Each record carries a distinct power of two, so a sum names exactly which records a range counted.
func TestRangesSumTheRightBuckets(t *testing.T) {
	type rec struct {
		at     string
		tokens int64
	}
	for _, tc := range []struct {
		name, zone, now string
		records         []rec
		want            map[Range]int64
		wantTodaySince  string
		wantTodayMins   int64
	}{
		{
			name: "plain day", zone: "Europe/Vienna", now: "2026-10-15 12:00",
			records: []rec{
				{"2026-10-08 12:05", 64}, // bucket starts exactly 7d ago: outside 7d
				{"2026-10-08 12:15", 32}, // first bucket inside 7d
				{"2026-10-14 23:59", 16}, // yesterday
				{"2026-10-15 00:00", 8},  // bucket starts at midnight: today
				{"2026-10-15 08:00", 4},  // today, before the live window
				{"2026-10-15 08:55", 2},  // oldest live-window bucket
				{"2026-10-15 11:55", 1},
			},
			want:           map[Range]int64{RangeWindow: 3, RangeToday: 15, Range7d: 63, RangeTotal: 127},
			wantTodaySince: "2026-10-15T00:00:00+02:00", wantTodayMins: 720,
		},
		{
			// +05:45: local midnight falls inside the bucket starting 18:10 UTC.
			name: "offset not aligned to buckets", zone: "Asia/Kathmandu", now: "2026-10-15 06:00",
			records: []rec{
				{"2026-10-14 23:58", 1},
				{"2026-10-15 00:02", 2}, // bucket started before midnight: not today
				{"2026-10-15 00:05", 4}, // first bucket starting after midnight
			},
			want:           map[Range]int64{RangeToday: 4, Range7d: 7, RangeTotal: 7},
			wantTodaySince: "2026-10-15T00:00:00+05:45", wantTodayMins: 360,
		},
		{
			name: "spring forward", zone: "Europe/Vienna", now: "2026-03-29 23:00",
			records: []rec{
				{"2026-03-28 23:50", 1},
				{"2026-03-29 00:30", 2},
				{"2026-03-29 03:10", 4},
			},
			want:           map[Range]int64{RangeToday: 6, RangeTotal: 7},
			wantTodaySince: "2026-03-29T00:00:00+01:00", wantTodayMins: 22 * 60,
		},
		{
			name: "fall back", zone: "Europe/Vienna", now: "2026-10-25 23:00",
			records: []rec{
				{"2026-10-24 23:50", 1},
				{"2026-10-25 00:30", 2},
				{"2026-10-25 02:30", 4},
			},
			want:           map[Range]int64{RangeToday: 6, RangeTotal: 7},
			wantTodaySince: "2026-10-25T00:00:00+02:00", wantTodayMins: 24 * 60,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := mustZone(t, tc.zone)
			clock := &fakeClock{}
			agg := New(clock.Now, 0)
			for _, r := range tc.records {
				clock.now = at(t, loc, r.at)
				agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1, TotalTokens: r.tokens})
			}
			clock.now = at(t, loc, tc.now)
			for r, want := range tc.want {
				snap := agg.Snapshot(r)
				if got := sumTokens(snap); got != want {
					t.Errorf("%s: tokens %d, want %d", r, got, want)
				}
				if snap.Range != r || snap.Timezone != tc.zone {
					t.Errorf("%s: range/timezone echo %q/%q", r, snap.Range, snap.Timezone)
				}
			}
			today := agg.Snapshot(RangeToday)
			if today.Since != tc.wantTodaySince || today.WindowMinutes != tc.wantTodayMins {
				t.Errorf("today since %s / %d min, want %s / %d", today.Since, today.WindowMinutes, tc.wantTodaySince, tc.wantTodayMins)
			}
		})
	}
}

func TestRangeSpans(t *testing.T) {
	loc := mustZone(t, "Europe/Vienna")
	clock := &fakeClock{now: at(t, loc, "2026-10-01 09:00")}
	agg := New(clock.Now, 0)
	agg.Add(Key{Provider: "p", Model: "m"}, Counters{Requests: 1})
	clock.now = at(t, loc, "2026-10-15 12:00")

	for _, tc := range []struct {
		r       Range
		since   string
		minutes int64
	}{
		{RangeWindow, "2026-10-15T08:40:00+02:00", 200},
		{Range7d, "2026-10-08T12:00:00+02:00", 7 * 24 * 60},
		{RangeTotal, "2026-10-01T09:00:00+02:00", 14*24*60 + 180},
	} {
		snap := agg.Snapshot(tc.r)
		if snap.Since != tc.since || snap.WindowMinutes != tc.minutes {
			t.Errorf("%s: since %s / %d min, want %s / %d", tc.r, snap.Since, snap.WindowMinutes, tc.since, tc.minutes)
		}
	}
}

func TestParseRange(t *testing.T) {
	for in, want := range map[string]Range{"": RangeWindow, "window": RangeWindow, "today": RangeToday, "7d": Range7d, "total": RangeTotal} {
		if got, err := ParseRange(in); err != nil || got != want {
			t.Errorf("ParseRange(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"week", "TOTAL", "1d"} {
		if _, err := ParseRange(in); err == nil {
			t.Errorf("ParseRange(%q) accepted", in)
		}
	}
}

func TestBucketsOlderThanSevenDaysLeave7dButStayInTotal(t *testing.T) {
	clock := newClock()
	agg := New(clock.Now, 0)
	key := Key{Provider: "p", Model: "m"}
	agg.Add(key, Counters{Requests: 1, TotalTokens: 5})
	clock.Advance(7 * 24 * time.Hour)
	agg.Add(key, Counters{Requests: 1, TotalTokens: 1}) // write prunes the old bucket

	if got := sumTokens(agg.Snapshot(Range7d)); got != 1 {
		t.Fatalf("7d tokens %d, want 1", got)
	}
	if got := findRow(t, agg.Snapshot(RangeTotal), "p", "m", false); got.Requests != 2 || got.TotalTokens != 6 {
		t.Fatalf("total %+v, want 2 requests / 6 tokens", got)
	}
}
