package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/snapshot"
)

// The restic engine stores a policy's backups in a restic repository at <store>/restic instead of as
// hardlinked folders, so a changed large file costs its changed chunks rather than a whole new copy.
// The tradeoff, stated plainly: the store folder is no longer browsable; getting data back needs restic.
//
// Every flag and JSON field used here was verified against restic 0.18.1, the package the runner image
// installs (the Dockerfile fails the build if it ever ships an older one). Do not add a flag without
// checking `restic <cmd> --help` against that version.

const resticDirName = "restic"

// resticRepo is this policy's repository inside its own mounted store directory. Per-policy repositories
// keep the tenant isolation the plain engine has: a mover only ever sees its own folder of the export.
func (r *runner) resticRepo() string { return filepath.Join(r.policyDir, resticDirName) }

// resticBase are the global flags of every restic command. --insecure-no-password creates and opens
// unencrypted repositories (restic >= 0.17): backshots deliberately has no password to lose on the worst
// day of the year, and the NFS export's ACL stays the security boundary, as it is for plain snapshots.
// The chunk index cache is large and regenerable, so it lives on the pod's scratch volume instead of
// filling the node's root disk; without a scratch mount --no-cache is safer than writing to the node.
func (r *runner) resticBase() []string {
	base := []string{"--repo", r.resticRepo(), "--insecure-no-password"}
	if fi, err := os.Stat(r.scratch); err == nil && fi.IsDir() {
		return append(base, "--cache-dir", filepath.Join(r.scratch, "restic-cache"))
	}
	return append(base, "--no-cache")
}

// resticRun runs one restic command with the base flags; dir is its working directory.
func (r *runner) resticRun(ctx context.Context, dir string, args ...string) (string, error) {
	out, _, err := run(ctx, proc{env: os.Environ(), dir: dir}, "restic", append(r.resticBase(), args...)...)
	return out, err
}

// resticSummary is the message_type=summary object restic backup prints with --json, and the summary each
// entry of `restic snapshots --json` carries. Only the fields backshots reads are declared.
type resticSummary struct {
	TotalFilesProcessed int64 `json:"total_files_processed"`
	TotalBytesProcessed int64 `json:"total_bytes_processed"`
	// DataAddedPacked is what the run really added to the repository after dedup: the number that shows
	// the engine did its job, so it goes into the status message.
	DataAddedPacked int64  `json:"data_added_packed"`
	SnapshotID      string `json:"snapshot_id"`
}

// resticSnapshot is one entry of `restic snapshots --json`.
type resticSnapshot struct {
	ShortID string        `json:"short_id"`
	ID      string        `json:"id"`
	Time    string        `json:"time"`
	Summary resticSummary `json:"summary"`
}

// resticSnapshots lists the repository's snapshots, oldest first.
func (r *runner) resticSnapshots(ctx context.Context) ([]resticSnapshot, error) {
	out, err := r.resticRun(ctx, "", "snapshots", "--json")
	if err != nil {
		return nil, err
	}
	var snaps []resticSnapshot
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		return nil, fmt.Errorf("parsing restic snapshots: %w", err)
	}
	sort.Slice(snaps, func(i, j int) bool {
		ti, e1 := time.Parse(time.RFC3339Nano, snaps[i].Time)
		tj, e2 := time.Parse(time.RFC3339Nano, snaps[j].Time)
		if e1 != nil || e2 != nil {
			return snaps[i].Time < snaps[j].Time
		}
		return ti.Before(tj)
	})
	return snaps, nil
}

// resticInit initialises the repository when it has no config yet; a mover owns its policy folder, so there
// is nothing else to prepare. init with --insecure-no-password creates a keyless repository.
func (r *runner) resticInit(ctx context.Context) error {
	repo := r.resticRepo()
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(repo, "config")); err == nil {
		return nil
	}
	r.logf("initialising restic repository %s", repo)
	_, err := r.resticRun(ctx, "", "init")
	return err
}

// resticUnlock clears locks left behind by a run that was killed mid-flight. Only stale ones matter: the
// controller guarantees that one policy never has two restic runs at once, and a live peer's lock would
// have failed this run anyway.
func (r *runner) resticUnlock(ctx context.Context) error {
	_, err := r.resticRun(ctx, "", "unlock")
	return err
}

// resticBackup runs one backup of src and returns its summary. The tree goes in as "." from the source
// root, which is what closes the round trip: `restic restore --target <same root>` lands the files back
// where they came from instead of nested one directory deeper. --host pins the hostname to the policy so
// runs on different nodes stay one history, and ownership, xattrs and ACLs are restic's own defaults.
func (r *runner) resticBackup(ctx context.Context, src string) (*resticSummary, error) {
	fs := r.cfg.Spec.Source.Files
	args := []string{"backup", "--json", "--host", r.cfg.Policy}
	for _, e := range fs.Exclude {
		args = append(args, "--exclude", e)
	}
	args = append(args, ".")
	r.logf("restic backup of %s -> %s", src, r.resticRepo())
	out, err := r.resticRun(ctx, src, args...)
	if err != nil {
		return nil, err
	}
	return parseResticBackupSummary(out)
}

// parseResticBackupSummary picks the summary object out of the JSON lines of `restic backup --json`.
func parseResticBackupSummary(out string) (*resticSummary, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var probe struct {
			MessageType string `json:"message_type"`
		}
		if json.Unmarshal([]byte(line), &probe) != nil || probe.MessageType != "summary" {
			continue
		}
		var sum resticSummary
		if err := json.Unmarshal([]byte(line), &sum); err != nil {
			return nil, fmt.Errorf("parsing restic backup summary: %w", err)
		}
		return &sum, nil
	}
	return nil, fmt.Errorf("restic backup printed no summary")
}

// resticForgetPrune applies the policy's resolved retention and prunes the freed chunks. Only counts > 0
// are passed, matching the plain engine: an absent count is "no constraint", not "keep none".
func (r *runner) resticForgetPrune(ctx context.Context) (kept, pruned int, err error) {
	ret := r.cfg.Retention
	if ret.IsZero() {
		ret = snapshot.Default // same fallback as prune() for the plain engine
	}
	args := []string{"forget", "--json"}
	for _, kd := range []struct {
		flag string
		n    int
	}{{"keep-hourly", ret.Hourly}, {"keep-daily", ret.Daily}, {"keep-weekly", ret.Weekly}, {"keep-monthly", ret.Monthly}} {
		if kd.n > 0 {
			args = append(args, "--"+kd.flag, strconv.Itoa(kd.n))
		}
	}
	args = append(args, "--prune")
	out, err := r.resticRun(ctx, "", args...)
	if err != nil {
		return 0, 0, err
	}
	// --json prints one array of forget groups first; the prune summary follows and is not needed.
	var groups []struct {
		Keep   []json.RawMessage `json:"keep"`
		Remove []json.RawMessage `json:"remove"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&groups); err != nil {
		return 0, 0, fmt.Errorf("parsing restic forget: %w", err)
	}
	for _, g := range groups {
		kept += len(g.Keep)
		pruned += len(g.Remove)
	}
	return kept, pruned, nil
}

// backupRestic is the restic engine's whole backup run: init, clear stale locks, back up, guard, retention.
func (r *runner) backupRestic(ctx context.Context, res *jobcfg.Result) error {
	if r.cfg.Spec.Source.Files == nil {
		return fmt.Errorf("source.files is not set")
	}
	src, err := r.sourceRoot()
	if err != nil {
		return err
	}
	if err := r.resticInit(ctx); err != nil {
		return err
	}
	if err := r.resticUnlock(ctx); err != nil {
		return err
	}
	// The guard compares against the previous snapshot's recorded file count, read from the repository.
	var prevFiles int64
	if snaps, err := r.resticSnapshots(ctx); err == nil && len(snaps) > 0 {
		prevFiles = snaps[len(snaps)-1].Summary.TotalFilesProcessed
	}
	sum, err := r.resticBackup(ctx, src)
	if err != nil {
		return err
	}
	if err := checkGuard(sum.TotalFilesProcessed, prevFiles, r.cfg.Spec.MinSourcePercent); err != nil {
		return err
	}
	res.Snapshot = shortResticID(sum.SnapshotID)
	res.Files, res.Bytes = sum.TotalFilesProcessed, sum.TotalBytesProcessed

	// Like the plain engine: the snapshot exists now, so housekeeping problems are warnings the next run
	// retries, not a failed run whose retry would back everything up again.
	var warnings []string
	kept, pruned, err := r.resticForgetPrune(ctx)
	res.Kept, res.Pruned = kept, pruned
	if err != nil {
		r.logf("WARNING: forget: %v", err)
		warnings = append(warnings, "forget failed: "+err.Error())
	}
	r.logf("retention: kept %d, pruned %d", kept, pruned)
	res.Message = fmt.Sprintf("restic snapshot %s: %d files, %s processed, %s added to repository; kept %d, pruned %d",
		res.Snapshot, res.Files, humanBytes(res.Bytes), humanBytes(sum.DataAddedPacked), kept, pruned)
	if len(warnings) > 0 {
		res.Message += " (warning: " + strings.Join(warnings, "; ") + ")"
	}
	return nil
}

// shortResticID abbreviates a snapshot id the way restic's own short ids do (8 hex characters).
func shortResticID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// humanBytes formats a byte count for the status message (1.5 MiB, 402 KiB, ...).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
