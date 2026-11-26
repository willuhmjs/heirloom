package runner

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

// stagingDirName is the transient folder in the policy's store directory where sqlite copies and
// postgres dumps wait for the commit into the restic repository. It lives on the store, not on pod
// scratch, because a dump set is sized by the databases and the store is the volume guaranteed to
// have room. Like the .incomplete- directories it replaces, it never needs to survive a run: the
// next backup sweeps a stale one before staging.
const stagingDirName = ".staging"

// backup commits one snapshot to the policy's restic repository: files sources are read in place
// from the volume, sqlite and postgres payloads are dumped to a staging folder first, but every
// source type then takes the same path — init, clear stale locks, back up with the guard, and apply
// retention.
func (r *runner) backup(ctx context.Context, res *jobcfg.Result) error {
	if err := r.resticInit(ctx); err != nil {
		return err
	}
	if err := r.resticUnlock(ctx); err != nil {
		return err
	}
	src, staging, err := r.backupSource(ctx)
	if err != nil {
		return err
	}
	if staging != "" {
		// Nothing to resume from after success or failure: restic has no partial-tree semantics,
		// the next run restages.
		defer func() { _ = os.RemoveAll(staging) }()
	}

	// The guard compares this run against the previous snapshot's file count, read from the repository.
	var prevFiles int64
	if snaps, err := r.resticSnapshots(ctx); err == nil && len(snaps) > 0 {
		prevFiles = snaps[len(snaps)-1].Summary.TotalFilesProcessed
	}
	sum, err := r.resticBackup(ctx, src)
	if err != nil {
		return err
	}
	if err := checkGuard(sum.TotalFilesProcessed, prevFiles, r.cfg.Spec.MinSourcePercent); err != nil {
		// restic has already committed the snapshot by now: a rejected run must not leave it behind.
		// A committed-but-rejected snapshot would be the repository's newest, so `restore from: latest`
		// would hand back the shrunken tree, and the next run would compare against it and stay silent
		// forever. Forget it so history still ends at the last good snapshot; the freed packs are
		// reclaimed by the next successful run's prune.
		if sum.SnapshotID != "" {
			if ferr := r.resticForgetIDs(ctx, sum.SnapshotID); ferr != nil {
				r.logf("WARNING: could not forget rejected snapshot %s: %v", shortResticID(sum.SnapshotID), ferr)
				return fmt.Errorf("%w; forgetting it also failed (%v): run `restic forget %s` in %s before restoring anything",
					err, ferr, sum.SnapshotID, r.resticRepo())
			}
			r.logf("forgot rejected snapshot %s; history still ends at the previous good snapshot", shortResticID(sum.SnapshotID))
		}
		return err
	}
	res.Snapshot = shortResticID(sum.SnapshotID)
	res.Files, res.Bytes = sum.TotalFilesProcessed, sum.TotalBytesProcessed
	res.Added = sum.DataAddedPacked

	// The snapshot is committed now, so housekeeping problems must not fail the run (the Job would be
	// retried and back everything up again); they are reported in the message and retried by the next run.
	var warnings []string
	kept, pruned, err := r.resticForgetPrune(ctx)
	res.Kept, res.Pruned = kept, pruned
	if err != nil {
		r.logf("WARNING: forget: %v", err)
		warnings = append(warnings, "forget failed: "+err.Error())
	}
	r.logf("retention: kept %d, pruned %d", kept, pruned)
	res.RepoBytes = r.resticRepoBytes(ctx) // best effort: a status nicety, never a reason to fail the run
	res.Message = fmt.Sprintf("restic snapshot %s: %d files, %s processed, %s added to repository; kept %d, pruned %d",
		res.Snapshot, res.Files, humanBytes(res.Bytes), humanBytes(sum.DataAddedPacked), kept, pruned)
	if len(warnings) > 0 {
		res.Message += " (warning: " + strings.Join(warnings, "; ") + ")"
	}
	return nil
}

// backupSource resolves the directory `restic backup .` will point at: the files volume (or the
// policy's subPath root) directly, or a freshly staged tree for sqlite and postgres payloads.
func (r *runner) backupSource(ctx context.Context) (src, staging string, err error) {
	switch r.cfg.Spec.Source.Type {
	case v1.SourceFiles:
		src, err = r.sourceRoot()
		return src, "", err
	case v1.SourceSQLite:
		if staging, err = r.beginStaging(); err != nil {
			return "", "", err
		}
		if err := r.captureSQLite(ctx, staging); err != nil {
			_ = os.RemoveAll(staging)
			return "", "", err
		}
		return staging, staging, nil
	case v1.SourcePostgres:
		if staging, err = r.beginStaging(); err != nil {
			return "", "", err
		}
		if err := r.capturePostgres(ctx, staging); err != nil {
			_ = os.RemoveAll(staging)
			return "", "", err
		}
		return staging, staging, nil
	case v1.SourceCommand:
		if staging, err = r.beginStaging(); err != nil {
			return "", "", err
		}
		if err := r.captureCommand(ctx, staging); err != nil {
			_ = os.RemoveAll(staging)
			return "", "", err
		}
		return staging, staging, nil
	}
	return "", "", fmt.Errorf("unknown source type %q", r.cfg.Spec.Source.Type)
}

// beginStaging wipes whatever a crashed run left behind and creates a fresh staging tree.
func (r *runner) beginStaging() (string, error) {
	stage := filepath.Join(r.policyDir, stagingDirName)
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	return stage, os.MkdirAll(stage, 0o755)
}

// checkGuard refuses to accept a snapshot of an empty or suspiciously small source.
func checkGuard(files, prevFiles int64, minPercent int32) error {
	if files == 0 {
		return guardError{"source has 0 files; refusing to snapshot an empty volume"}
	}
	if minPercent > 0 && prevFiles > 0 && files*100 < prevFiles*int64(minPercent) {
		return guardError{fmt.Sprintf("source has %d files, less than %d%% of the previous snapshot's %d; refusing (set minSourcePercent: 0 to override)",
			files, minPercent, prevFiles)}
	}
	return nil
}

// guardError marks a run that failed the empty/shrunken-source check, as opposed to one that was interrupted.
type guardError struct{ msg string }

func (e guardError) Error() string { return e.msg }

// sourceRoot is the directory a files backup captures: the volume root, or the policy's subPath
// inside it, resolved with containment so a planted symlink cannot point the backup (or a restore)
// outside the volume.
func (r *runner) sourceRoot() (string, error) {
	src := r.source
	fs := r.cfg.Spec.Source.Files
	if fs != nil && fs.SubPath != "" && fs.SubPath != "." && fs.SubPath != "/" {
		sub, err := cleanRel(fs.SubPath)
		if err != nil {
			return "", fmt.Errorf("subPath: %w", err)
		}
		if src, err = resolveWithin(r.source, sub); err != nil {
			return "", fmt.Errorf("subPath: %w", err)
		}
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			return "", fmt.Errorf("subPath %q is not a directory in the volume", sub)
		}
	}
	return src, nil
}

// captureSQLite writes consistent sqlite copies under stage/sqlite, the layout a restore reads
// back out of the snapshot. Committing them is the backup run's job.
func (r *runner) captureSQLite(ctx context.Context, stage string) error {
	sq := r.cfg.Spec.Source.SQLite
	if sq == nil || len(sq.Paths) == 0 {
		return fmt.Errorf("source.sqlite.paths is empty")
	}
	root := filepath.Join(stage, "sqlite")
	for _, p := range sq.Paths {
		rel, err := cleanRel(p)
		if err != nil {
			return err
		}
		srcDB, err := resolveWithin(r.source, rel)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		r.logf("sqlite backup %s", rel)
		q := ".backup '" + strings.ReplaceAll(dst, "'", "''") + "'"
		if _, _, err := run(ctx, proc{env: os.Environ()}, "sqlite3", srcDB, q); err != nil {
			return fmt.Errorf("backup %s: %w", rel, err)
		}
		if err := sqliteCheck(ctx, dst); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if f, err := os.Open(dst); err == nil { // make sure it reached the NFS server
			_ = f.Sync()
			_ = f.Close()
		}
	}
	return nil
}

// sqliteCheck runs integrity_check on a finished copy. It opens the file immutable: a copy taken
// from a WAL database is still in WAL mode, and a normal open would try to create -wal/-shm
// beside it — dirtying a copy that must stay byte-identical to what gets committed (and failing
// outright where the tree is mounted read-only). Copies are never modified once written.
func sqliteCheck(ctx context.Context, path string) error {
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "immutable=1"}).String()
	out, _, err := run(ctx, proc{env: os.Environ()}, "sqlite3", uri, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("integrity_check failed: %s", tail(strings.TrimSpace(out), 500))
	}
	return nil
}
