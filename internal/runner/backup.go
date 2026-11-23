package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/snapshot"
)

func (r *runner) backup(ctx context.Context, res *jobcfg.Result) error {
	now := r.now().UTC()
	name := snapshot.Name(now)
	inc := filepath.Join(r.policyDir, snapshot.IncompletePrefix+name)
	final := filepath.Join(r.policyDir, name)
	if r.cfg.Spec.Source.Type == v1alpha1.SourceFiles {
		if err := r.resumePartial(inc); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(inc, 0o755); err != nil {
		return err
	}
	st := snapshot.Status{Snapshot: name, Type: string(r.cfg.Spec.Source.Type), Time: now}
	if err := r.capture(ctx, inc, &st); err != nil {
		// A files run that failed or was killed at its deadline leaves a partial tree. Keep it: the next run
		// resumes from it, so a first backup too large for one deadline still completes. Only a run that failed
		// its own sanity check (an empty or shrunken source) has a tree that must not be reused.
		var ge guardError
		if r.cfg.Spec.Source.Type != v1alpha1.SourceFiles || errors.As(err, &ge) {
			os.RemoveAll(inc)
		}
		return err
	}
	if err := writeStatus(inc, st); err != nil {
		os.RemoveAll(inc)
		return err
	}
	if err := os.Rename(inc, final); err != nil {
		os.RemoveAll(inc)
		return fmt.Errorf("publishing snapshot: %w", err)
	}
	r.logf("snapshot %s complete: %d files, %d bytes", name, st.Files, st.Bytes)
	res.Snapshot, res.Files, res.Bytes = name, st.Files, st.Bytes

	// The snapshot is published and complete. Housekeeping problems must not fail the run (the Job would be
	// retried and rsync everything again); they are reported in the message and retried by the next run.
	var warnings []string
	kept, pruned, err := prune(r.policyDir, r.cfg.Retention)
	res.Kept, res.Pruned = kept, pruned
	if err != nil {
		r.logf("WARNING: prune: %v", err)
		warnings = append(warnings, "prune failed: "+err.Error())
	}
	if removed, err := cleanIncomplete(r.policyDir, now, incompleteMaxAge); err != nil {
		r.logf("WARNING: incomplete cleanup: %v", err)
		warnings = append(warnings, "incomplete cleanup failed: "+err.Error())
	} else if len(removed) > 0 {
		r.logf("removed stale incomplete dirs: %s", strings.Join(removed, ", "))
	}
	r.logf("retention: kept %d, pruned %d", kept, pruned)
	res.Message = fmt.Sprintf("snapshot %s: %d files, %d bytes; kept %d, pruned %d", name, st.Files, st.Bytes, kept, pruned)
	if len(warnings) > 0 {
		res.Message += " (warning: " + strings.Join(warnings, "; ") + ")"
	}
	return nil
}

func (r *runner) capture(ctx context.Context, inc string, st *snapshot.Status) error {
	switch r.cfg.Spec.Source.Type {
	case v1alpha1.SourceFiles:
		return r.captureFiles(ctx, inc, st)
	case v1alpha1.SourceSQLite:
		return r.captureSQLite(ctx, inc, st)
	case v1alpha1.SourcePostgres:
		return r.capturePostgres(ctx, inc, st)
	}
	return fmt.Errorf("unknown source type %q", r.cfg.Spec.Source.Type)
}

// checkGuard refuses to publish a snapshot of an empty or suspiciously small source.
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

// resumePartial adopts the newest unfinished tree of an earlier files run as the destination of this one. rsync
// skips what is already there, so a run killed at its deadline loses no transferred data, and --delete brings the
// tree in line with the source as it is now.
func (r *runner) resumePartial(inc string) error {
	ents, err := os.ReadDir(r.policyDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	newest := ""
	for _, e := range ents {
		path := filepath.Join(r.policyDir, e.Name())
		if e.IsDir() && snapshot.IsIncomplete(e.Name()) && path != inc && e.Name() > newest {
			newest = e.Name()
		}
	}
	if newest == "" {
		return nil
	}
	r.logf("resuming the unfinished run %s", strings.TrimPrefix(newest, snapshot.IncompletePrefix))
	return os.Rename(filepath.Join(r.policyDir, newest), inc)
}

func (r *runner) captureFiles(ctx context.Context, inc string, st *snapshot.Status) error {
	fs := r.cfg.Spec.Source.Files
	if fs == nil {
		return fmt.Errorf("source.files is not set")
	}
	src := r.source
	if fs.SubPath != "" && fs.SubPath != "." && fs.SubPath != "/" {
		sub, err := cleanRel(fs.SubPath)
		if err != nil {
			return fmt.Errorf("subPath: %w", err)
		}
		if src, err = resolveWithin(r.source, sub); err != nil {
			return fmt.Errorf("subPath: %w", err)
		}
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			return fmt.Errorf("subPath %q is not a directory in the volume", sub)
		}
	}
	dst := filepath.Join(inc, "data")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	args := []string{rsyncFlags(fs.SkipACLsAndXattrs), "-S", "--no-D", "--numeric-ids", "--delete"}
	for _, e := range fs.Exclude {
		args = append(args, "--exclude="+e)
	}
	var prevFiles int64
	if names, err := listSnapshots(r.policyDir); err == nil && len(names) > 0 {
		prev := filepath.Join(r.policyDir, names[len(names)-1])
		if ps, err := readStatus(prev); err == nil {
			prevFiles = ps.Files
		}
		if _, err := os.Stat(filepath.Join(prev, "data")); err == nil {
			args = append(args, "--link-dest="+filepath.Join(prev, "data")+"/")
			r.logf("hardlinking against %s", names[len(names)-1])
		}
	}
	args = append(args, src+"/", dst+"/")
	r.logf("rsync %s -> %s", src, dst)
	if err := rsync(ctx, args); err != nil {
		return err
	}
	files, bytes, err := countTree(dst)
	if err != nil {
		return err
	}
	if err := checkGuard(files, prevFiles, r.cfg.Spec.MinSourcePercent); err != nil {
		return err
	}
	st.Files, st.Bytes, st.Items = files, bytes, []string{"data"}
	return nil
}

// rsyncArchive is the archive flag set; tests override it for rsync builds without -A/-X (macOS openrsync).
var rsyncArchive = "-aHAX"

// rsyncFlags is the archive flag set: rsyncArchive, or without ACLs and extended attributes when the NFS export lacks them.
func rsyncFlags(skipACLs bool) string {
	if skipACLs {
		return "-aH"
	}
	return rsyncArchive
}

// protectExcludes returns rsync filter rules that keep paths the backup excludes from being removed by a restore with --delete.
func protectExcludes(files *v1alpha1.FilesSource) []string {
	if files == nil {
		return nil
	}
	var rules []string
	for _, e := range files.Exclude {
		rules = append(rules, "--filter=P "+e)
	}
	return rules
}

// rsync runs rsync, treating exit 24 (files vanished during the transfer) as success.
func rsync(ctx context.Context, args []string) error {
	_, _, err := run(ctx, proc{env: os.Environ()}, "rsync", args...)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 24 { // 24: files vanished while copying, harmless
		return nil
	}
	return err
}

func (r *runner) captureSQLite(ctx context.Context, inc string, st *snapshot.Status) error {
	sq := r.cfg.Spec.Source.SQLite
	if sq == nil || len(sq.Paths) == 0 {
		return fmt.Errorf("source.sqlite.paths is empty")
	}
	for _, p := range sq.Paths {
		rel, err := cleanRel(p)
		if err != nil {
			return err
		}
		srcDB, err := resolveWithin(r.source, rel)
		if err != nil {
			return err
		}
		dst := filepath.Join(inc, "sqlite", rel)
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
			f.Sync()
			f.Close()
		}
		st.Items = append(st.Items, rel)
	}
	files, bytes, err := countTree(filepath.Join(inc, "sqlite"))
	st.Files, st.Bytes = files, bytes
	return err
}

// sqliteCheck runs PRAGMA integrity_check on a database copy.
// sqliteCheck runs integrity_check on a finished copy. It opens the file immutable: a copy taken from a WAL
// database is still in WAL mode, and a normal open would need to create -wal/-shm, which fails on the
// read-only store mount that verify uses. Snapshots never change after the atomic rename, so immutable is accurate.
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
