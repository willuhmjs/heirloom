package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/willuhmjs/heirloom/internal/snapshot"
)

// Backups live in a restic repository at <store>/<policy>/restic: content-addressed, deduplicated
// pack files instead of browsable folders, so a changed large file costs its changed chunks rather
// than a whole new copy. The tradeoff, stated plainly: the store folder is no longer browsable;
// getting data back needs restic.
//
// Every flag and JSON field used here was verified against restic 0.18.1, the version the runner image
// builds in (the Dockerfile fails the build if it ever ships an older or broken one). Do not add a flag
// without checking `restic <cmd> --help` against that version.

const resticDirName = "restic"

// resticBin is the restic binary to exec: the copy `heirloom install` leaves next to this executable
// when the mover runs inside the user's postgres image (which ships no restic), else the one on PATH.
// Static and version-identical either way, so behaviour cannot drift between mover container images.
var resticBin = sync.OnceValue(func() string {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "restic")
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
	}
	return "restic"
})

// resticRepo is this policy's repository inside its own mounted store directory. Per-policy repositories
// it keeps the operator's tenant isolation: a mover only ever sees its own folder of the export.
func (r *runner) resticRepo() string { return filepath.Join(r.policyDir, resticDirName) }

// resticBase are the global flags of every restic command. --insecure-no-password creates and opens
// unencrypted repositories (restic >= 0.17): heirloom deliberately has no password to lose on the worst
// day of the year, and the NFS export's ACL stays the security boundary, as it is for plain snapshots.
// The chunk index cache is large and regenerable, so it lives on the pod's scratch volume instead of
// filling the node's root disk; without a scratch mount --no-cache is safer than writing to the node.
func (r *runner) resticBase() []string {
	base := []string{"--repo", r.resticRepo()}
	if !r.cfg.Encrypted {
		base = append(base, "--insecure-no-password")
	}
	if fi, err := os.Stat(r.scratch); err == nil && fi.IsDir() {
		return append(base, "--cache-dir", filepath.Join(r.scratch, "restic-cache"))
	}
	return append(base, "--no-cache")
}

// resticRun runs one restic command with the base flags; dir is its working directory.
func (r *runner) resticRun(ctx context.Context, dir string, args ...string) (string, error) {
	out, _, err := run(ctx, proc{env: os.Environ(), dir: dir}, resticBin(), append(r.resticBase(), args...)...)
	return out, err
}

// resticRepoBytes is the repository's raw data size from `restic stats`, or 0 when it cannot be read.
func (r *runner) resticRepoBytes(ctx context.Context) int64 {
	out, err := r.resticRun(ctx, "", "stats", "--mode", "raw-data", "--json")
	if err != nil {
		r.logf("WARNING: repository size: %v", err)
		return 0
	}
	var st struct {
		TotalSize int64 `json:"total_size"`
	}
	if json.Unmarshal([]byte(out), &st) != nil {
		return 0
	}
	return st.TotalSize
}

// resticSummary is the message_type=summary object restic backup prints with --json, and the summary each
// entry of `restic snapshots --json` carries. Only the fields heirloom reads are declared.
type resticSummary struct {
	TotalFilesProcessed int64 `json:"total_files_processed"`
	TotalBytesProcessed int64 `json:"total_bytes_processed"`
	// DataAddedPacked is what the run really added to the repository after dedup: the number that shows
	// dedup did its job, so it goes into the status message.
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

// resticUnlock clears locks left behind by a run that was killed mid-flight. Only stale ones matter:
// the controller guarantees that one policy never has two restic runs at once (a Restore waits for
// active backup/verify Jobs and scheduling pauses while a Restore runs), so any live lock found here
// belongs to nobody and every mode — backup, verify and restore — clears one before touching the
// repository.
func (r *runner) resticUnlock(ctx context.Context) error {
	_, err := r.resticRun(ctx, "", "unlock")
	return err
}

// resticForgetIDs drops the given snapshots from the repository's history without pruning. This is
// how a committed-but-rejected snapshot is rolled back: leaving its reference in place would keep it
// the newest thing restic restores from; the freed pack files are reclaimed by the next
// forget --prune.
func (r *runner) resticForgetIDs(ctx context.Context, ids ...string) error {
	_, err := r.resticRun(ctx, "", append([]string{"forget"}, ids...)...)
	return err
}

// resticBackup runs one backup of src and returns its summary. The tree goes in as "." from the
// source root, which is what closes the round trip: `restic restore --target <same root>` lands the
// files back where they came from instead of nested one directory deeper. --host pins the hostname
// to the policy so runs on different nodes stay one history, and ownership, xattrs and ACLs are
// restic's own defaults.
func (r *runner) resticBackup(ctx context.Context, src string) (*resticSummary, error) {
	args := []string{"backup", "--json", "--host", r.cfg.Policy}
	if fs := r.cfg.Spec.Source.Files; fs != nil {
		for _, e := range fs.Exclude {
			args = append(args, "--exclude", e)
		}
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
// are passed: an absent count is "no constraint", not "keep none".
func (r *runner) resticForgetPrune(ctx context.Context) (kept, pruned int, err error) {
	ret := r.cfg.Retention
	if ret.IsZero() {
		ret = snapshot.Default // the same fallback the pre-restic prune() used
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

// resticResolve picks the snapshot to read: "latest" (or unset) or an explicit id. The controller has
// already validated the shape; the repository decides whether it exists.
func (r *runner) resticResolve(ctx context.Context, from string) (*resticSnapshot, error) {
	snaps, err := r.resticSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("the restic repository in %s holds no snapshots", r.resticRepo())
	}
	if from == "" || from == "latest" {
		return &snaps[len(snaps)-1], nil
	}
	if !snapshot.ResticID(from) {
		return nil, fmt.Errorf("from %q is not \"latest\" or a restic snapshot id", from)
	}
	for i := range snaps {
		if snaps[i].ShortID == from || snaps[i].ID == from || strings.HasPrefix(snaps[i].ID, from) {
			return &snaps[i], nil
		}
	}
	return nil, fmt.Errorf("restic snapshot %q not found", from)
}

// resticCheckData proves the repository structurally, then reads one share of the pack data. The
// share rotates by ISO week number, so every pack is read within ten weeks even when verify only
// runs monthly. Neither command writes to the repository; scratch only holds the index cache, so a
// verify run (whose store mount is read-only) needs no unlock and no writable repository.
func (r *runner) resticCheckData(ctx context.Context) (string, error) {
	if _, err := r.resticRun(ctx, "", "check"); err != nil {
		return "", err
	}
	_, week := r.now().UTC().ISOWeek()
	subset := fmt.Sprintf("%d/%d", (week-1)%resticVerifySubsets+1, resticVerifySubsets)
	if _, err := r.resticRun(ctx, "", "check", "--read-data-subset="+subset); err != nil {
		return subset, fmt.Errorf("read-data-subset %s: %w", subset, err)
	}
	return subset, nil
}

// resticVerifySampleFiles restores the sample files into scratch and checksum-compares each against
// `restic dump`, returning how many files were proven.
func (r *runner) resticVerifySampleFiles(ctx context.Context, id string) (int, error) {
	samples, err := r.resticSamples(ctx, id)
	if err != nil {
		return 0, err
	}
	if err := r.resticSampleChecksums(ctx, id, samples); err != nil {
		return 0, err
	}
	return len(samples), nil
}

const (
	// resticVerifySubsets splits the pack data into ten rotating shares: one ISO week checks 1/10 of the
	// packs, so the whole repository is read within ten weeks even with a monthly verify schedule.
	resticVerifySubsets = 10
	resticVerifySamples = 5
	// resticVerifySampleMax caps a sample file's size: verify streams it twice through memory for the
	// checksum comparison, and a sample only has to prove decodability, not throughput.
	resticVerifySampleMax = 4 << 20
)

// resticSamples picks up to resticVerifySamples files from the snapshot tree: the smallest first, so the
// checksum pass costs a bounded amount of memory and time no matter how big the volume is.
func (r *runner) resticSamples(ctx context.Context, id string) ([]resticNode, error) {
	out, err := r.resticRun(ctx, "", "ls", id, "--json")
	if err != nil {
		return nil, err
	}
	var files []resticNode
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var n resticNode
		if json.Unmarshal([]byte(line), &n) != nil || n.MessageType != "node" || n.Type != "file" {
			continue
		}
		if n.Size > 0 && n.Size <= resticVerifySampleMax {
			files = append(files, n)
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].Size < files[j].Size })
	if len(files) > resticVerifySamples {
		files = files[:resticVerifySamples]
	}
	return files, nil
}

// resticNode is one line of `restic ls --json` (only snapshot and node lines matter).
type resticNode struct {
	MessageType string `json:"message_type"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
}

// resticSampleChecksums restores the sample files into scratch and compares each against the sha256 of
// the same file streamed through `restic dump`. Two independent read paths through the pack files must
// agree bit for bit; a corrupted pack fails one of them (or the check that ran first).
func (r *runner) resticSampleChecksums(ctx context.Context, id string, samples []resticNode) error {
	if len(samples) == 0 {
		r.logf("restic verify: no files small enough to sample")
		return nil
	}
	stage := filepath.Join(r.scratch, "restic-verify")
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	args := []string{"restore", id, "--target", stage}
	for _, n := range samples {
		args = append(args, "--include", n.Path)
	}
	if _, err := r.resticRun(ctx, "", args...); err != nil {
		return err
	}
	for _, n := range samples {
		restored := filepath.Join(stage, filepath.FromSlash(strings.TrimPrefix(n.Path, "/")))
		h, err := fileSHA256(restored)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Path, err)
		}
		out, err := r.resticRun(ctx, "", "dump", id, n.Path)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Path, err)
		}
		if got := sha256Hex([]byte(out)); got != h {
			return fmt.Errorf("%s: restored file sha256 %s, dump produced %s", n.Path, h, got)
		}
	}
	r.logf("restic verify: %d sample files restored and checksummed", len(samples))
	return nil
}

// fileSHA256 streams a file through sha256 and returns the lowercase hex digest.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sha256Hex is fileSHA256 for bytes already in memory.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
