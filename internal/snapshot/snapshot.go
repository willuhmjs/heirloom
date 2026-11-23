// Package snapshot holds the on-disk layout and retention logic. It has no Kubernetes dependencies.
//
// Layout under the store root:
//
//	<namespace>/<policy>/<snapshot>/        complete snapshot, e.g. 2026-11-22T10-05-00Z
//	<namespace>/<policy>/.incomplete-<snapshot>/   run in progress or crashed; never browsed or restored
//
// A snapshot directory only ever appears via an atomic rename of a finished .incomplete- directory,
// so every visible snapshot is complete.
package snapshot

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	TimeLayout       = "2006-01-02T15-04-05Z"
	IncompletePrefix = ".incomplete-"
	// DeletingPrefix marks a snapshot that prune has renamed and is removing. It no longer parses as a snapshot.
	DeletingPrefix = ".deleting-"
	StatusFile     = "_status.json"
)

// Name formats t (converted to UTC) as a snapshot directory name.
func Name(t time.Time) string { return t.UTC().Format(TimeLayout) }

// Parse returns the time encoded in a snapshot directory name.
func Parse(name string) (time.Time, bool) {
	t, err := time.Parse(TimeLayout, name)
	return t, err == nil
}

// resticID is a restic snapshot id: lowercase hex, 8 characters (as restic itself prints) up to the full 64.
var resticID = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// ResticID reports whether s is a restic snapshot id rather than a snapshot directory name.
func ResticID(s string) bool { return resticID.MatchString(s) }

// IsIncomplete reports whether a directory name is an unfinished snapshot.
func IsIncomplete(name string) bool { return strings.HasPrefix(name, IncompletePrefix) }

// Sorted returns the valid snapshot names from names, oldest first.
func Sorted(names []string) []string {
	var out []string
	for _, n := range names {
		if _, ok := Parse(n); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Retention mirrors the API type without importing it.
type Retention struct{ Hourly, Daily, Weekly, Monthly int }

// Default is used when neither the policy nor the store sets retention.
var Default = Retention{Hourly: 24, Daily: 30, Weekly: 12, Monthly: 24}

func (r Retention) IsZero() bool { return r == Retention{} }

// Keep returns the snapshot names to retain. For each bucket kind (hour, day, ISO week, month) it keeps
// the newest snapshot of each of the N most recent distinct buckets that contain a snapshot. The newest
// snapshot overall is always kept. Names that do not parse are ignored (and so are never pruned by callers
// that only delete names absent from the returned set among parsed ones).
func Keep(names []string, r Retention) map[string]bool {
	all := Sorted(names)
	keep := map[string]bool{}
	if len(all) == 0 {
		return keep
	}
	keep[all[len(all)-1]] = true

	kinds := []struct {
		n   int
		key func(time.Time) string
	}{
		{r.Hourly, func(t time.Time) string { return t.Format("2006-01-02T15") }},
		{r.Daily, func(t time.Time) string { return t.Format("2006-01-02") }},
		{r.Weekly, func(t time.Time) string {
			y, w := t.ISOWeek()
			return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006") + "-W" + pad2(w)
		}},
		{r.Monthly, func(t time.Time) string { return t.Format("2006-01") }},
	}
	for _, k := range kinds {
		if k.n <= 0 {
			continue
		}
		seen := map[string]bool{}
		for i := len(all) - 1; i >= 0 && len(seen) < k.n; i-- { // newest first
			t, _ := Parse(all[i])
			b := k.key(t)
			if seen[b] {
				continue
			}
			seen[b] = true
			keep[all[i]] = true // first hit per bucket scanning newest-first is the newest in that bucket
		}
	}
	return keep
}

func pad2(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// Prune returns the snapshot names that retention allows deleting, oldest first.
func Prune(names []string, r Retention) []string {
	keep := Keep(names, r)
	var del []string
	for _, n := range Sorted(names) {
		if !keep[n] {
			del = append(del, n)
		}
	}
	return del
}

// Status is written as _status.json inside every snapshot and is the run's result.
type Status struct {
	Snapshot string    `json:"snapshot"`
	Type     string    `json:"type"`
	Time     time.Time `json:"time"`
	Bytes    int64     `json:"bytes"`
	Files    int64     `json:"files"`
	// Items lists what was captured: databases, sqlite paths, or "data".
	Items []string `json:"items,omitempty"`
}
