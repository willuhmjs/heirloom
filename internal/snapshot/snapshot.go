// Package snapshot holds the retention model shared by the API and the mover, plus the snapshot-id
// shape the Restore API accepts. It has no Kubernetes dependencies.
//
// Backups themselves are restic repositories (see internal/runner): one per policy, inside the
// policy's own store folder. Retention counts map onto `restic forget --keep-*`, and a snapshot is
// identified by its restic hex id rather than a folder name.
package snapshot

import "regexp"

// Retention is how many snapshots to keep per bucket, applied with `restic forget`.
type Retention struct{ Hourly, Daily, Weekly, Monthly int }

// Default is the retention a policy or store gets when it sets none.
var Default = Retention{Hourly: 24, Daily: 30, Weekly: 12, Monthly: 24}

// IsZero reports whether no count is set; callers then fall back to Default.
func (r Retention) IsZero() bool { return r == Retention{} }

// resticID is a restic snapshot id: lowercase hex, 8 characters (as restic itself prints) up to the full 64.
var resticID = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// ResticID reports whether s is a restic snapshot id rather than "latest".
func ResticID(s string) bool { return resticID.MatchString(s) }
