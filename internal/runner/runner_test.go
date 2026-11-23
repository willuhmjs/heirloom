package runner

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/snapshot"
)

func mkdir(t *testing.T, p ...string) string {
	t.Helper()
	d := filepath.Join(p...)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func write(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCleanRel(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "../x", "a/../../x", "/../x", "a/.."} {
		if _, err := cleanRel(bad); err == nil {
			t.Errorf("cleanRel(%q) should fail", bad)
		}
	}
	got, err := cleanRel("/a//b/./c")
	if err != nil || got != "a/b/c" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSelectSnapshot(t *testing.T) {
	dir := t.TempDir()
	if _, err := selectSnapshot(dir, "latest"); err == nil {
		t.Error("empty dir should fail")
	}
	for _, n := range []string{"2026-01-01T00-00-00Z", "2026-01-03T00-00-00Z", "2026-01-02T00-00-00Z", ".incomplete-2026-01-09T00-00-00Z", "junk"} {
		mkdir(t, dir, n)
	}
	if got, err := selectSnapshot(dir, "latest"); err != nil || got != "2026-01-03T00-00-00Z" {
		t.Errorf("latest = %q, %v", got, err)
	}
	if got, err := selectSnapshot(dir, ""); err != nil || got != "2026-01-03T00-00-00Z" {
		t.Errorf("empty = %q, %v", got, err)
	}
	if got, err := selectSnapshot(dir, "2026-01-02T00-00-00Z"); err != nil || got != "2026-01-02T00-00-00Z" {
		t.Errorf("named = %q, %v", got, err)
	}
	for _, bad := range []string{"2026-01-09T00-00-00Z", "../x", ".incomplete-2026-01-09T00-00-00Z", "junk", "2026-01-01T00-00-00Z/.."} {
		if _, err := selectSnapshot(dir, bad); err == nil {
			t.Errorf("selectSnapshot(%q) should fail", bad)
		}
	}
}

func TestCheckGuard(t *testing.T) {
	cases := []struct {
		files, prev int64
		pct         int32
		fail        bool
	}{
		{0, 0, 0, true},
		{0, 100, 50, true},
		{1, 0, 50, false},
		{49, 100, 50, true},
		{50, 100, 50, false},
		{1, 100, 0, false},
	}
	for _, c := range cases {
		if err := checkGuard(c.files, c.prev, c.pct); (err != nil) != c.fail {
			t.Errorf("%+v: err=%v", c, err)
		}
	}
}

func TestCleanIncomplete(t *testing.T) {
	dir := t.TempDir()
	old := mkdir(t, dir, ".incomplete-2026-01-01T00-00-00Z")
	fresh := mkdir(t, dir, ".incomplete-2026-01-02T00-00-00Z")
	done := mkdir(t, dir, "2026-01-01T00-00-00Z")
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, past, past)
	os.Chtimes(done, past, past)
	removed, err := cleanIncomplete(dir, time.Now(), 24*time.Hour)
	if err != nil || len(removed) != 1 {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old incomplete dir should be gone")
	}
	for _, p := range []string{fresh, done} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should remain", p)
		}
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ { // ten daily snapshots
		mkdir(t, dir, snapshot.Name(start.AddDate(0, 0, i)))
	}
	mkdir(t, dir, ".incomplete-x")
	kept, pruned, err := prune(dir, snapshot.Retention{Daily: 3})
	if err != nil || kept != 3 || pruned != 7 {
		t.Fatalf("kept=%d pruned=%d err=%v", kept, pruned, err)
	}
	names, _ := listSnapshots(dir)
	if len(names) != 3 || names[2] != "2026-01-10T00-00-00Z" {
		t.Errorf("names = %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, ".incomplete-x")); err != nil {
		t.Error("prune must not touch incomplete dirs")
	}
}

func TestWriteResult(t *testing.T) {
	p := filepath.Join(t.TempDir(), "term")
	writeResult(p, jobcfg.Result{Mode: jobcfg.ModeBackup, OK: true, Message: strings.Repeat("x", 10000), Files: 3})
	b, _ := os.ReadFile(p)
	if len(b) > 4096 {
		t.Errorf("termination log is %d bytes", len(b))
	}
	var got jobcfg.Result
	if err := json.Unmarshal(b, &got); err != nil || !got.OK || got.Files != 3 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestExecuteFailureWritesResult(t *testing.T) {
	t.Setenv(EnvTerminationLog, filepath.Join(t.TempDir(), "term"))
	cfg := jobcfg.Config{Mode: "bogus"}
	enc, _ := cfg.Encode()
	t.Setenv(jobcfg.EnvConfig, enc)
	if code := Run(t.Context()); code != 1 {
		t.Errorf("exit code %d", code)
	}
	b, _ := os.ReadFile(os.Getenv(EnvTerminationLog))
	var res jobcfg.Result
	json.Unmarshal(b, &res)
	if res.OK || !strings.Contains(res.Message, "bogus") {
		t.Errorf("result %+v", res)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := snapshot.Status{Snapshot: "s", Type: "files", Time: time.Unix(100, 0).UTC(), Files: 2, Bytes: 9, Items: []string{"data"}}
	if err := writeStatus(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := readStatus(dir)
	if err != nil || out.Files != 2 || out.Items[0] != "data" || !out.Time.Equal(in.Time) {
		t.Errorf("%+v %v", out, err)
	}
}

// ---- integration tests (need rsync / sqlite3) ----

func need(t *testing.T, bin string) {
	t.Helper()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not installed", bin)
	}
}

// testRunner builds a runner on temp dirs; the clock advances one hour per call so snapshot names differ.
func testRunner(t *testing.T, mode jobcfg.Mode, spec v1alpha1.BackupPolicySpec) *runner {
	t.Helper()
	root := t.TempDir()
	r := newRunner(jobcfg.Config{Mode: mode, Namespace: "ns", Policy: "p", Spec: spec,
		Retention: snapshot.Retention{Hourly: 100}})
	r.policyDir = mkdir(t, root, "store")
	r.source = mkdir(t, root, "source")
	r.scratch = mkdir(t, root, "scratch")
	r.out = &bytes.Buffer{}
	tm := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { tm = tm.Add(time.Hour); return tm }
	return r
}

func (r *runner) as(mode jobcfg.Mode, rs *jobcfg.Restore) *runner {
	c := *r
	c.cfg.Mode, c.cfg.Restore = mode, rs
	return &c
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

func TestFilesBackupVerifyRestore(t *testing.T) {
	need(t, "rsync")
	if _, _, err := run(t.Context(), proc{}, "rsync", "-A", "--version"); err != nil {
		old := rsyncArchive
		rsyncArchive = "-aH" // no ACL/xattr support here
		t.Cleanup(func() { rsyncArchive = old })
	}
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		MinSourcePercent: 50,
		Source:           v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "x", Exclude: []string{"*.tmp"}}},
	})
	write(t, filepath.Join(r.source, "a.txt"), "aaa")
	write(t, filepath.Join(r.source, "sub/b.txt"), "bbbb")
	write(t, filepath.Join(r.source, "skip.tmp"), "zzz")

	res1 := r.execute(t.Context())
	if !res1.OK {
		t.Fatal(res1.Message)
	}
	if res1.Files != 2 || res1.Bytes != 7 {
		t.Errorf("res1 = %+v", res1)
	}
	write(t, filepath.Join(r.source, "sub/b.txt"), "BBBBB")
	res2 := r.execute(t.Context())
	if !res2.OK {
		t.Fatal(res2.Message)
	}
	s1, s2 := filepath.Join(r.policyDir, res1.Snapshot, "data"), filepath.Join(r.policyDir, res2.Snapshot, "data")
	if inode(t, filepath.Join(s1, "a.txt")) != inode(t, filepath.Join(s2, "a.txt")) {
		t.Error("unchanged file should be hardlinked between snapshots")
	}
	if inode(t, filepath.Join(s1, "sub/b.txt")) == inode(t, filepath.Join(s2, "sub/b.txt")) {
		t.Error("changed file must not share an inode")
	}
	if _, err := os.Stat(filepath.Join(s2, "skip.tmp")); err == nil {
		t.Error("excluded file was copied")
	}

	if v := r.as(jobcfg.ModeVerify, nil).execute(t.Context()); !v.OK || v.Snapshot != res2.Snapshot {
		t.Errorf("verify: %+v", v)
	}

	// Guard: an emptied source must not produce a snapshot.
	os.RemoveAll(filepath.Join(r.source, "sub"))
	os.Remove(filepath.Join(r.source, "a.txt"))
	if g := r.execute(t.Context()); g.OK {
		t.Error("empty source should be refused")
	}
	if names, _ := listSnapshots(r.policyDir); len(names) != 2 {
		t.Errorf("snapshots = %v", names)
	}
	ents, _ := os.ReadDir(r.policyDir)
	for _, e := range ents {
		if snapshot.IsIncomplete(e.Name()) {
			t.Errorf("leftover %s", e.Name())
		}
	}

	// Restore the older snapshot, deleting extras.
	write(t, filepath.Join(r.source, "extra.txt"), "x")
	rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: res1.Snapshot, Delete: true}).execute(t.Context())
	if !rr.OK {
		t.Fatal(rr.Message)
	}
	if b, _ := os.ReadFile(filepath.Join(r.source, "sub/b.txt")); string(b) != "bbbb" {
		t.Errorf("restored b.txt = %q", b)
	}
	if _, err := os.Stat(filepath.Join(r.source, "extra.txt")); err == nil {
		t.Error("--delete should remove extra.txt")
	}

	// Path-filtered restore and traversal rejection.
	os.Remove(filepath.Join(r.source, "a.txt"))
	if rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest", Paths: []string{"a.txt"}}).execute(t.Context()); !rr.OK {
		t.Fatal(rr.Message)
	}
	if _, err := os.Stat(filepath.Join(r.source, "a.txt")); err != nil {
		t.Error("a.txt not restored")
	}
	if rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest", Paths: []string{"../etc"}}).execute(t.Context()); rr.OK {
		t.Error("traversal should fail")
	}
}

func TestFilesRestoreHonorsSubPath(t *testing.T) {
	need(t, "rsync")
	if _, _, err := run(t.Context(), proc{}, "rsync", "-A", "--version"); err != nil {
		old := rsyncArchive
		rsyncArchive = "-aH" // no ACL/xattr support here
		t.Cleanup(func() { rsyncArchive = old })
	}
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "x", SubPath: "git"}},
	})
	write(t, filepath.Join(r.source, "git/a.txt"), "A")
	write(t, filepath.Join(r.source, "other/keep.txt"), "K")

	res := r.execute(t.Context())
	if !res.OK {
		t.Fatal(res.Message)
	}

	t.Run("whole snapshot", func(t *testing.T) {
		os.Remove(filepath.Join(r.source, "git/a.txt"))
		rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest", Delete: true}).execute(t.Context())
		if !rr.OK {
			t.Fatal(rr.Message)
		}
		if b, _ := os.ReadFile(filepath.Join(r.source, "git/a.txt")); string(b) != "A" {
			t.Errorf("restored git/a.txt = %q", b)
		}
		if b, err := os.ReadFile(filepath.Join(r.source, "other/keep.txt")); err != nil || string(b) != "K" {
			t.Errorf("--delete outside subPath clobbered other/keep.txt: %q, %v", b, err)
		}
		if _, err := os.Stat(filepath.Join(r.source, "a.txt")); err == nil {
			t.Error("file restored to volume root instead of subPath")
		}
	})

	t.Run("paths", func(t *testing.T) {
		os.Remove(filepath.Join(r.source, "git/a.txt"))
		rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest", Paths: []string{"a.txt"}}).execute(t.Context())
		if !rr.OK {
			t.Fatal(rr.Message)
		}
		if b, _ := os.ReadFile(filepath.Join(r.source, "git/a.txt")); string(b) != "A" {
			t.Errorf("restored git/a.txt = %q", b)
		}
	})
}

func TestSQLiteBackupVerifyRestore(t *testing.T) {
	need(t, "sqlite3")
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"db/app.db"}}},
	})
	mkdir(t, r.source, "db")
	db := filepath.Join(r.source, "db/app.db")
	if _, _, err := run(t.Context(), proc{env: os.Environ()}, "sqlite3", db, "create table t(v); insert into t values('one');"); err != nil {
		t.Fatal(err)
	}
	res := r.execute(t.Context())
	if !res.OK || res.Files != 1 {
		t.Fatalf("backup: %+v", res)
	}
	if v := r.as(jobcfg.ModeVerify, nil).execute(t.Context()); !v.OK {
		t.Fatalf("verify: %+v", v)
	}
	// Change the db, add stale wal/shm, restore.
	os.Chmod(db, 0o600)
	run(t.Context(), proc{env: os.Environ()}, "sqlite3", db, "insert into t values('two');")
	write(t, db+"-wal", "stale")
	write(t, db+"-shm", "stale")
	if rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest"}).execute(t.Context()); !rr.OK {
		t.Fatalf("restore: %+v", rr)
	}
	out, _, err := run(t.Context(), proc{env: os.Environ()}, "sqlite3", db, "select count(*) from t")
	if err != nil || strings.TrimSpace(out) != "1" {
		t.Errorf("count = %q, %v", out, err)
	}
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(db + s); err == nil {
			t.Errorf("%s not removed", s)
		}
	}
	if info, _ := os.Stat(db); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}
	if rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest", Paths: []string{"../x"}}).execute(t.Context()); rr.OK {
		t.Error("traversal should fail")
	}
}

func TestMaintenanceDB(t *testing.T) {
	if got := maintenanceDB("postgres.sql.gz"); got != "template1" {
		t.Fatalf("postgres dump must load via template1, got %s", got)
	}
	for _, f := range []string{"app.sql.gz", "globals.sql.gz"} {
		if got := maintenanceDB(f); got != "postgres" {
			t.Fatalf("%s loaded via %s", f, got)
		}
	}
}

func TestResolveWithin(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mustWrite := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(root, "real", "file"))
	mustWrite(filepath.Join(outside, "secret"))
	for name, target := range map[string]string{"abs": outside, "rel": "../" + filepath.Base(outside)} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("real", filepath.Join(root, "inner")); err != nil { // symlink that stays inside is fine
		t.Fatal(err)
	}

	for _, rel := range []string{"abs", "abs/secret", "rel", "rel/secret", "abs/new/file"} {
		if _, err := resolveWithin(root, rel); err == nil {
			t.Errorf("%q escapes the volume but was accepted", rel)
		}
	}
	for _, rel := range []string{"real/file", "inner/file", "real/not/yet/created"} {
		got, err := resolveWithin(root, rel)
		if err != nil {
			t.Errorf("%q: %v", rel, err)
			continue
		}
		rootReal, _ := filepath.EvalSymlinks(root)
		if !strings.HasPrefix(got, rootReal+"/real") {
			t.Errorf("%q resolved to %q", rel, got)
		}
	}
}

func TestVerifyDatabaseIsNeverGuessed(t *testing.T) {
	pg := func(dbs ...string) v1alpha1.BackupPolicySpec {
		return v1alpha1.BackupPolicySpec{Source: v1alpha1.Source{Postgres: &v1alpha1.PostgresSource{Databases: dbs}}}
	}
	if _, err := verifyDatabase(pg(), []string{"app", "other", "postgres"}); err == nil {
		t.Error("several databases and no verify.database must be an error")
	}
	if got, err := verifyDatabase(pg("app"), []string{"app"}); err != nil || got != "app" {
		t.Errorf("single listed database: %q %v", got, err)
	}
	s := pg()
	s.Verify = &v1alpha1.Verify{Database: "other"}
	if got, err := verifyDatabase(s, []string{"app", "other"}); err != nil || got != "other" {
		t.Errorf("explicit database: %q %v", got, err)
	}
	if _, err := verifyDatabase(s, []string{"app"}); err == nil {
		t.Error("verify.database absent from the snapshot must be an error")
	}
}

// A symlink planted in a user-writable volume must not let a backup or restore touch anything outside it.
func TestSymlinkEscapeIsRefused(t *testing.T) {
	outside := mkdir(t, t.TempDir(), "other-namespace-backups")
	write(t, filepath.Join(outside, "secret.db"), "secret")

	t.Run("files subPath", func(t *testing.T) {
		r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
			Source: v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "x", SubPath: "link"}},
		})
		if err := os.Symlink(outside, filepath.Join(r.source, "link")); err != nil {
			t.Fatal(err)
		}
		res := r.execute(t.Context())
		if res.OK || !strings.Contains(res.Message, "outside its volume") {
			t.Fatalf("symlinked subPath accepted: %+v", res)
		}
		if names, _ := listSnapshots(r.policyDir); len(names) != 0 {
			t.Fatalf("a snapshot was created: %v", names)
		}
	})

	t.Run("sqlite path", func(t *testing.T) {
		r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
			Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"link/secret.db"}}},
		})
		if err := os.Symlink(outside, filepath.Join(r.source, "link")); err != nil {
			t.Fatal(err)
		}
		res := r.execute(t.Context())
		if res.OK || !strings.Contains(res.Message, "outside its volume") {
			t.Fatalf("symlinked sqlite path accepted: %+v", res)
		}
	})

	t.Run("restore destination", func(t *testing.T) {
		r := testRunner(t, jobcfg.ModeRestore, v1alpha1.BackupPolicySpec{
			Source: v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "x"}},
		})
		snap := filepath.Join(r.policyDir, "2026-01-01T00-00-00Z")
		write(t, filepath.Join(snap, "data", "link", "evil"), "payload")
		if err := os.Symlink(outside, filepath.Join(r.source, "link")); err != nil {
			t.Fatal(err)
		}
		res := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest", Paths: []string{"link/evil"}}).execute(t.Context())
		if res.OK {
			t.Fatalf("restore through a symlink succeeded: %+v", res)
		}
		if _, err := os.Stat(filepath.Join(outside, "evil")); err == nil {
			t.Fatal("restore wrote outside the volume")
		}
	})
}

// Verify mounts the store read-only, and a copy taken from a WAL database is still in WAL mode: opening it
// normally would need to create -wal/-shm. It must still pass.
func TestSQLiteCheckWorksOnReadOnlyWALCopy(t *testing.T) {
	need(t, "sqlite3")
	dir := t.TempDir()
	db := filepath.Join(dir, "app.db")
	if out, err := exec.Command("sqlite3", db, "pragma journal_mode=wal; create table t(x); insert into t values (1);").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	_ = os.Remove(db + "-wal")
	_ = os.Remove(db + "-shm")
	if err := os.Chmod(dir, 0o555); err != nil { // read-only directory, like the verify mount
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := sqliteCheck(t.Context(), db); err != nil {
		t.Fatalf("integrity check on a read-only WAL copy: %v", err)
	}
}

func TestRsyncMissingSourceIsAFailure(t *testing.T) {
	need(t, "rsync")
	dst := t.TempDir()
	if err := rsync(t.Context(), []string{"-a", "/nonexistent-backshots-path/", dst + "/"}); err == nil {
		t.Fatal("rsync with a missing source must fail, not be treated as success")
	}
}

func TestRsyncFlags(t *testing.T) {
	if got := rsyncFlags(true); got != "-aH" {
		t.Errorf("rsyncFlags(true) = %q", got)
	}
	if got := rsyncFlags(false); got != rsyncArchive {
		t.Errorf("rsyncFlags(false) = %q, want %q", got, rsyncArchive)
	}
}

func TestRestoreDeleteKeepsBackupExcludes(t *testing.T) {
	need(t, "rsync")
	if _, _, err := run(t.Context(), proc{}, "rsync", "-A", "--version"); err != nil {
		old := rsyncArchive
		rsyncArchive = "-aH" // no ACL/xattr support here
		t.Cleanup(func() { rsyncArchive = old })
	}
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "x", Exclude: []string{"cache"}}},
	})
	write(t, filepath.Join(r.source, "a.txt"), "A")
	write(t, filepath.Join(r.source, "cache/c.tmp"), "C")
	if res := r.execute(t.Context()); !res.OK {
		t.Fatal(res.Message)
	}
	write(t, filepath.Join(r.source, "junk.txt"), "J") // not in the snapshot and not excluded
	rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest", Delete: true}).execute(t.Context())
	if !rr.OK {
		t.Fatal(rr.Message)
	}
	if _, err := os.Stat(filepath.Join(r.source, "cache/c.tmp")); err != nil {
		t.Errorf("restore --delete removed a path the backup excludes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.source, "junk.txt")); err == nil {
		t.Error("restore --delete should remove files that are neither in the snapshot nor excluded")
	}
}

// An interrupted prune must never leave a half-deleted directory that still looks like a snapshot.
func TestPruneRenamesBeforeDeletingAndSweepsLeftovers(t *testing.T) {
	dir := t.TempDir()
	old := "2026-01-01T00-00-00Z"
	newer := "2026-01-02T00-00-00Z"
	for _, n := range []string{old, newer} {
		write(t, filepath.Join(dir, n, "data", "f"), n)
		write(t, filepath.Join(dir, n, snapshot.StatusFile), "{}")
	}
	// Left behind by a prune that died after the rename but before the delete.
	write(t, filepath.Join(dir, snapshot.DeletingPrefix+"2025-12-31T00-00-00Z", "data", "f"), "x")

	kept, pruned, err := prune(dir, snapshot.Retention{Daily: 1})
	if err != nil || kept != 1 || pruned != 1 {
		t.Fatalf("kept=%d pruned=%d err=%v", kept, pruned, err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != newer {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("left in dir: %v, want only %s", names, newer)
	}
}

// A prune problem after a published snapshot must not fail the run.
func TestBackupSucceedsWhenPruneFails(t *testing.T) {
	need(t, "sqlite3")
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"a.db"}}},
	})
	if out, err := exec.Command("sqlite3", filepath.Join(r.source, "a.db"), "create table t(x); insert into t values (1);").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if res := r.execute(t.Context()); !res.OK {
		t.Fatal(res.Message)
	}
	// Make the old snapshot undeletable by planting a non-empty directory the process cannot remove.
	if os.Geteuid() == 0 {
		t.Skip("root can remove anything")
	}
	names, _ := listSnapshots(r.policyDir)
	locked := filepath.Join(r.policyDir, names[0], "locked")
	write(t, filepath.Join(locked, "f"), "x")
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	// The locked directory is renamed along with its snapshot, so re-enable permissions recursively.
	t.Cleanup(func() { exec.Command("chmod", "-R", "u+rwx", r.policyDir).Run() })
	r.cfg.Retention = snapshot.Retention{Hourly: 1}
	res := r.execute(t.Context())
	if !res.OK || !strings.Contains(res.Message, "warning") {
		t.Fatalf("a prune failure must be a warning, not a failed run: %+v", res)
	}
}

// A rollback journal left by the old database is "hot" for the restored file: SQLite would roll it back
// onto the restored data. It must be removed like -wal and -shm.
func TestSQLiteRestoreRemovesAllStaleSidecars(t *testing.T) {
	need(t, "sqlite3")
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"a.db"}}},
	})
	db := filepath.Join(r.source, "a.db")
	if out, err := exec.Command("sqlite3", db, "create table t(x); insert into t values (1);").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if res := r.execute(t.Context()); !res.OK {
		t.Fatal(res.Message)
	}
	for _, s := range []string{"-wal", "-shm", "-journal"} {
		write(t, db+s, "stale")
	}
	rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest"}).execute(t.Context())
	if !rr.OK {
		t.Fatal(rr.Message)
	}
	for _, s := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(db + s); err == nil {
			t.Errorf("stale %s survived the restore", s)
		}
	}
}

// Restoring into a volume that has no database yet must not leave it owned by root.
func TestSQLiteRestoreIntoNewVolumeInheritsDirectoryOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create files owned by another user")
	}
	need(t, "sqlite3")
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"a.db"}}},
	})
	if out, err := exec.Command("sqlite3", filepath.Join(r.source, "a.db"), "create table t(x);").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if res := r.execute(t.Context()); !res.OK {
		t.Fatal(res.Message)
	}
	os.Remove(filepath.Join(r.source, "a.db"))
	if err := os.Chown(r.source, 1234, 1234); err != nil {
		t.Fatal(err)
	}
	rr := r.as(jobcfg.ModeRestore, &jobcfg.Restore{From: "latest"}).execute(t.Context())
	if !rr.OK {
		t.Fatal(rr.Message)
	}
	info, err := os.Stat(filepath.Join(r.source, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 1234 || st.Gid != 1234 {
		t.Errorf("restored db owned by %d:%d, want 1234:1234", st.Uid, st.Gid)
	}
}

func TestValidDBNameRejectsGlobalsCollision(t *testing.T) {
	if err := validDBName("globals"); err == nil {
		t.Error(`a database named "globals" would be overwritten by the roles dump and must be rejected`)
	}
	for _, ok := range []string{"app", "my db", "Blog-2"} {
		if err := validDBName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestLoadGlobalsToleratesSnapshotWithoutRoles(t *testing.T) {
	// With globals: skip a snapshot has no globals.sql.gz; verify and restore must not fail on that.
	if err := loadGlobals(t.Context(), nil, t.TempDir()); err != nil {
		t.Fatalf("missing globals dump must be skipped: %v", err)
	}
}

// The dump stops when its destination fails, instead of blocking on a full pipe until the Job deadline.
func TestDumpGzipFailsFastWhenDestinationFails(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("no /dev/full")
	}
	done := make(chan error, 1)
	go func() {
		// yes writes forever; /dev/full makes every write fail with ENOSPC.
		done <- dumpGzip(t.Context(), proc{env: os.Environ()}, "/dev/full", "yes")
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("dumpGzip hung after the destination failed")
	}
}

// A first backup too big for one deadline must still finish: the next run adopts the unfinished tree and
// rsync skips what is already there instead of copying everything again.
func TestFilesBackupResumesAnUnfinishedRun(t *testing.T) {
	need(t, "rsync")
	if _, _, err := run(t.Context(), proc{}, "rsync", "-A", "--version"); err != nil {
		old := rsyncArchive
		rsyncArchive = "-aH" // no ACL/xattr support here
		t.Cleanup(func() { rsyncArchive = old })
	}
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "x"}},
	})
	write(t, filepath.Join(r.source, "big.bin"), "already copied")
	write(t, filepath.Join(r.source, "late.txt"), "not copied yet")

	// What a run killed at its deadline leaves behind: one file transferred (same size and mtime), one not.
	partial := filepath.Join(r.policyDir, snapshot.IncompletePrefix+"2026-01-01T00-00-00Z")
	if err := os.MkdirAll(filepath.Join(partial, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-p", filepath.Join(r.source, "big.bin"), filepath.Join(partial, "data", "big.bin")).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	write(t, filepath.Join(partial, "data", "stale.txt"), "no longer in the source")
	ino := inode(t, filepath.Join(partial, "data", "big.bin"))

	res := r.execute(t.Context())
	if !res.OK {
		t.Fatal(res.Message)
	}
	snap := filepath.Join(r.policyDir, res.Snapshot, "data")
	if got := inode(t, filepath.Join(snap, "big.bin")); got != ino {
		t.Error("the file already in the partial tree was copied again instead of being reused")
	}
	if b, _ := os.ReadFile(filepath.Join(snap, "late.txt")); string(b) != "not copied yet" {
		t.Error("the missing file was not transferred")
	}
	if _, err := os.Stat(filepath.Join(snap, "stale.txt")); err == nil {
		t.Error("a file that is no longer in the source survived in the snapshot")
	}
	if _, err := os.Stat(partial); err == nil {
		t.Error("the partial directory was not consumed")
	}
}

// An interrupted run's tree is kept for resuming, but one that failed the empty-source check is not.
func TestFailedFilesRunKeepsPartialUnlessGuardTripped(t *testing.T) {
	need(t, "rsync")
	if _, _, err := run(t.Context(), proc{}, "rsync", "-A", "--version"); err != nil {
		old := rsyncArchive
		rsyncArchive = "-aH" // no ACL/xattr support here
		t.Cleanup(func() { rsyncArchive = old })
	}
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		MinSourcePercent: 50,
		Source:           v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "x"}},
	})
	// The guard trips on an empty source: no partial tree may be left to resume from.
	if res := r.execute(t.Context()); res.OK {
		t.Fatal("an empty source must fail")
	}
	ents, _ := os.ReadDir(r.policyDir)
	for _, e := range ents {
		if snapshot.IsIncomplete(e.Name()) {
			t.Errorf("guard failure left %s behind", e.Name())
		}
	}
}

func TestErrorTextDoesNotQuoteRowData(t *testing.T) {
	in := "psql:<stdin>:21: ERROR:  duplicate key value violates unique constraint \"users_email_key\"\n" +
		"DETAIL:  Key (email)=(alice@example.com) already exists.\n" +
		"CONTEXT:  COPY users, line 3: \"1\talice@example.com\tsecret\"\n" +
		"STATEMENT:  INSERT INTO users VALUES (1, 'alice@example.com')\n" +
		"psql:<stdin>:22: QUERY:  SELECT 'x'\n" +
		"HINT:  something helpful"
	got := tail(in, 4000)
	for _, leak := range []string{"alice@example.com", "secret", "INSERT INTO", "DETAIL", "CONTEXT", "STATEMENT", "QUERY"} {
		if strings.Contains(got, leak) {
			t.Errorf("message still contains %q:\n%s", leak, got)
		}
	}
	for _, want := range []string{"duplicate key value violates unique constraint", "HINT:  something helpful"} {
		if !strings.Contains(got, want) {
			t.Errorf("message lost %q:\n%s", want, got)
		}
	}
}
