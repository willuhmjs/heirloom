package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
)

// restore writes one snapshot from the policy's restic repository back to the volume: files trees
// are written in place, sqlite and postgres payloads are first materialized into scratch, where
// each source's own load logic picks them up.
func (r *runner) restore(ctx context.Context, res *jobcfg.Result) error {
	rs := r.cfg.Restore
	if rs == nil {
		return fmt.Errorf("restore options missing from config")
	}
	snap, err := r.resticResolve(ctx, rs.From)
	if err != nil {
		return err
	}
	res.Snapshot = snap.ShortID
	res.Files, res.Bytes = snap.Summary.TotalFilesProcessed, snap.Summary.TotalBytesProcessed

	switch r.cfg.Spec.Source.Type {
	case v1alpha1.SourceFiles:
		err = r.restoreFiles(ctx, snap)
	case v1alpha1.SourceSQLite:
		var stage string
		if stage, err = r.stageSnapshot(ctx, snap); err == nil {
			err = r.restoreSQLite(stage)
		}
	case v1alpha1.SourcePostgres:
		var stage string
		if stage, err = r.stageSnapshot(ctx, snap); err == nil {
			err = r.restorePostgres(ctx, stage)
		}
	default:
		err = fmt.Errorf("unknown source type %q", r.cfg.Spec.Source.Type)
	}
	if err != nil {
		return err
	}
	res.Message = "restored restic snapshot " + snap.ShortID
	return nil
}

// restoreFiles restores the files tree onto the volume root (or the policy's subPath root): the
// tree was stored as "." from exactly that directory, so files land back where they came from.
// With delete the backup's exclude patterns are passed along too — restic's --delete honours them,
// which is how a restic restore keeps the invariant that a restore never removes paths the backup
// excluded.
func (r *runner) restoreFiles(ctx context.Context, snap *resticSnapshot) error {
	rs := r.cfg.Restore
	dst, err := r.sourceRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	args := []string{"restore", snap.ID, "--target", dst}
	for _, p := range rs.Paths {
		rel, err := cleanRel(p)
		if err != nil {
			return err
		}
		// Anchored at the snapshot root ("/" prefix) so only the requested path matches, never a same-named
		// directory elsewhere in the tree.
		args = append(args, "--include", "/"+rel)
	}
	if rs.Delete {
		args = append(args, "--delete")
		if fs := r.cfg.Spec.Source.Files; fs != nil {
			for _, e := range fs.Exclude {
				args = append(args, "--exclude", e)
			}
		}
	}
	r.logf("restic restore %s -> %s", snap.ShortID, dst)
	if _, err := r.resticRun(ctx, "", args...); err != nil {
		return err
	}
	return nil
}

// stageSnapshot materializes a sqlite/postgres snapshot's payload into scratch and returns the
// directory restoreSQLite/restorePostgres read; they expect the staging layout (sqlite/... and
// postgres/... directly beneath it), which is how the payload was committed. Database restores
// always go through scratch: the store mount is read-only during restore and verify, and scratch
// is where the user promised the space (pod.scratchStorageClassName).
func (r *runner) stageSnapshot(ctx context.Context, snap *resticSnapshot) (string, error) {
	stage := filepath.Join(r.scratch, "restic-restore")
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", err
	}
	args := []string{"restore", snap.ID, "--target", stage}
	switch r.cfg.Spec.Source.Type {
	case v1alpha1.SourceSQLite:
		sq := r.cfg.Spec.Source.SQLite
		if sq == nil {
			return "", fmt.Errorf("source.sqlite is not set")
		}
		paths := sq.Paths
		if rs := r.cfg.Restore; rs != nil && len(rs.Paths) > 0 {
			paths = rs.Paths
		}
		for _, p := range paths {
			rel, err := cleanRel(p)
			if err != nil {
				return "", err
			}
			args = append(args, "--include", "/sqlite/"+rel)
		}
	case v1alpha1.SourcePostgres:
		// With an explicit database list, restore only those dumps; a full restore (no list) pulls
		// the whole tree because restorePostgres also loads the globals from it.
		if rs := r.cfg.Restore; rs != nil {
			for _, db := range rs.Databases {
				if err := validDBName(db); err != nil {
					return "", err
				}
				args = append(args, "--include", "/postgres/"+db+".sql.gz")
			}
		}
	}
	r.logf("restic restore %s -> %s", snap.ShortID, stage)
	if _, err := r.resticRun(ctx, "", args...); err != nil {
		return "", err
	}
	return stage, nil
}

func (r *runner) restoreSQLite(dir string) error {
	sq := r.cfg.Spec.Source.SQLite
	if sq == nil {
		return fmt.Errorf("source.sqlite is not set")
	}
	paths := sq.Paths
	if len(r.cfg.Restore.Paths) > 0 {
		paths = r.cfg.Restore.Paths
	}
	for _, p := range paths {
		rel, err := cleanRel(p)
		if err != nil {
			return err
		}
		src, err := resolveWithin(filepath.Join(dir, "sqlite"), rel)
		if err != nil {
			return err
		}
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("database %q is not in the snapshot: %w", rel, err)
		}
		dst, err := resolveWithin(r.source, rel)
		if err != nil {
			return err
		}
		r.logf("restoring %s", rel)
		if err := replaceFile(src, dst); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		// A stale WAL/SHM or rollback journal would be replayed on top of the restored database and corrupt it.
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if err := os.Remove(dst + suffix); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// replaceFile copies src over dst via a temp file and rename, keeping the owner and mode of an existing dst.
func replaceFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".backshots-restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	// Keep the owner and mode of the database being replaced. A database that did not exist (restore into a new
	// volume) inherits the owner of its directory rather than becoming root's: the app has to be able to write it.
	mode := os.FileMode(0o644)
	var owner os.FileInfo
	if info, err := os.Stat(dst); err == nil {
		mode, owner = info.Mode().Perm(), info
	} else if dirInfo, err := os.Stat(filepath.Dir(dst)); err == nil {
		owner = dirInfo
	}
	if owner != nil {
		if sys, ok := owner.Sys().(*syscall.Stat_t); ok {
			if err := tmp.Chown(int(sys.Uid), int(sys.Gid)); err != nil && os.Geteuid() == 0 {
				tmp.Close()
				return err
			}
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (r *runner) restorePostgres(ctx context.Context, dir string) error {
	if r.cfg.Spec.Source.Postgres == nil {
		return fmt.Errorf("source.postgres is not set")
	}
	pgdir := filepath.Join(dir, "postgres")
	env := r.pgEnv()
	dbs := r.cfg.Restore.Databases
	if len(dbs) == 0 {
		r.logf("restoring globals")
		if err := loadGlobals(ctx, env, pgdir); err != nil {
			return err
		}
		var err error
		if dbs, err = dumpDatabases(pgdir); err != nil {
			return err
		}
	}
	for _, db := range dbs {
		if err := validDBName(db); err != nil {
			return err
		}
		file := filepath.Join(pgdir, db+".sql.gz")
		if _, err := os.Stat(file); err != nil {
			return fmt.Errorf("database %q is not in the snapshot", db)
		}
		r.logf("restoring database %s (connections to it must be closed)", db)
		if err := loadDump(ctx, env, file, false); err != nil {
			return err
		}
	}
	return nil
}
