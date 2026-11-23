package snapshot

import (
	"testing"
	"time"
)

func mk(start time.Time, step time.Duration, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, Name(start.Add(time.Duration(i)*step)))
	}
	return out
}

func TestNameRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 5, 0, 0, time.UTC)
	got, ok := Parse(Name(now))
	if !ok || !got.Equal(now) {
		t.Fatalf("round trip: %v %v", got, ok)
	}
	if _, ok := Parse(".incomplete-2026-11-22T10-05-00Z"); ok {
		t.Fatal("incomplete dir must not parse as snapshot")
	}
}

func TestKeepAlwaysNewest(t *testing.T) {
	names := mk(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Hour, 5)
	k := Keep(names, Retention{}) // zero retention still keeps newest
	if len(k) != 1 || !k[names[4]] {
		t.Fatalf("got %v", k)
	}
}

func TestKeepHourlyAndDaily(t *testing.T) {
	// 10 days of hourly snapshots.
	names := mk(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Hour, 240)
	k := Keep(names, Retention{Hourly: 24, Daily: 5})
	// 24 newest hours + daily newest-per-day for last 5 days. Day buckets overlap with hours for the
	// newest day(s), so count distinct.
	want := map[string]bool{}
	for _, n := range names[len(names)-24:] {
		want[n] = true
	}
	for d := 0; d < 5; d++ { // last snapshot of each of last 5 days
		want[names[len(names)-1-(d*24)-0]] = true
	}
	// last snapshot of day is 23:00; the newest day is partial only if the series ends mid-day.
	if len(k) < 24 || len(k) > 24+5 {
		t.Fatalf("kept %d", len(k))
	}
	for n := range want {
		if !k[n] && n == names[len(names)-1] {
			t.Fatalf("newest missing")
		}
	}
}

func TestKeepMonthlySparse(t *testing.T) {
	// one snapshot per month for 30 months, keep 24 monthly => 24 kept
	var names []string
	d := time.Date(2024, 1, 15, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		names = append(names, Name(d.AddDate(0, i, 0)))
	}
	k := Keep(names, Retention{Monthly: 24})
	if len(k) != 24 {
		t.Fatalf("kept %d want 24", len(k))
	}
	if k[names[0]] || !k[names[29]] {
		t.Fatal("wrong ends kept")
	}
}

func TestKeepBucketKeepsNewestInBucket(t *testing.T) {
	names := []string{
		Name(time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)),
		Name(time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)),
		Name(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)),
	}
	k := Keep(names, Retention{Daily: 3})
	if k[names[0]] || !k[names[1]] || !k[names[2]] {
		t.Fatalf("got %v", k)
	}
}

func TestPruneIgnoresGarbage(t *testing.T) {
	names := append(mk(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Hour, 3), "lost+found", ".incomplete-x")
	for _, p := range Prune(names, Retention{Hourly: 1}) {
		if _, ok := Parse(p); !ok {
			t.Fatalf("pruned non-snapshot %q", p)
		}
	}
}

func TestPruneISOWeekYearBoundary(t *testing.T) {
	// 2026-12-31 and 2027-01-01 are in the same ISO week (2026-W53); must count as one weekly bucket.
	names := []string{
		Name(time.Date(2026, 12, 31, 10, 0, 0, 0, time.UTC)),
		Name(time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC)),
	}
	if len(Keep(names, Retention{Weekly: 5})) != 1 {
		t.Fatal("ISO week across year boundary should be a single bucket")
	}
}
