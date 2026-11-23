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

func (r *runner) restore(ctx context.Context, res *jobcfg.Result) error {
	rs := r.cfg.Restore
	if rs == nil {
		return fmt.Errorf("restore options missing from config")
	}
	name, err := selectSnapshot(r.policyDir, rs.From)
	if err != nil {
		return err
	}
	res.Snapshot = name
	dir := filepath.Join(r.policyDir, name)
	st, err := readStatus(dir)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", name, err)
	}
	res.Files, res.Bytes = st.Files, st.Bytes
	r.logf("restoring from snapshot %s", name)

	switch r.cfg.Spec.Source.Type {
	case v1alpha1.SourceFiles:
		err = r.restoreFiles(ctx, dir)
	case v1alpha1.SourceSQLite:
		err = r.restoreSQLite(dir)
	case v1alpha1.SourcePostgres:
		err = r.restorePostgres(ctx, dir)
	default:
		err = fmt.Errorf("unknown source type %q", r.cfg.Spec.Source.Type)
	}
	if err != nil {
		return err
	}
	res.Message = "restored snapshot " + name
	return nil
}

func (r *runner) restoreFiles(ctx context.Context, dir string) error {
	rs := r.cfg.Restore
	data := filepath.Join(dir, "data")
	files := r.cfg.Spec.Source.Files
	base := []string{rsyncFlags(files != nil && files.SkipACLsAndXattrs), "-S", "--no-D", "--numeric-ids"}
	// The snapshot holds the contents of the policy's subPath, so they restore back onto that subPath.
	dstRoot := r.source
	if fs := r.cfg.Spec.Source.Files; fs != nil && fs.SubPath != "" && fs.SubPath != "." && fs.SubPath != "/" {
		sub, err := cleanRel(fs.SubPath)
		if err != nil {
			return fmt.Errorf("subPath: %w", err)
		}
		if dstRoot, err = resolveWithin(r.source, sub); err != nil {
			return err
		}
		if err := os.MkdirAll(dstRoot, 0o755); err != nil {
			return err
		}
	}
	if len(rs.Paths) == 0 {
		if rs.Delete {
			base = append(base, "--delete")
			base = append(base, protectExcludes(files)...)
		}
		return rsync(ctx, append(base, data+"/", dstRoot+"/"))
	}
	for _, p := range rs.Paths {
		rel, err := cleanRel(p)
		if err != nil {
			return err
		}
		src, err := resolveWithin(data, rel)
		if err != nil {
			return err
		}
		dst, err := resolveWithin(dstRoot, rel)
		if err != nil {
			return err
		}
		info, err := os.Lstat(src)
		if err != nil {
			return fmt.Errorf("path %q is not in the snapshot: %w", rel, err)
		}
		args := append([]string{}, base...)
		if info.IsDir() {
			if rs.Delete {
				args = append(args, "--delete")
				args = append(args, protectExcludes(files)...)
			}
			src, dst = src+"/", dst+"/"
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		r.logf("restoring %s", rel)
		if err := rsync(ctx, append(args, src, dst)); err != nil {
			return err
		}
	}
	return nil
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
