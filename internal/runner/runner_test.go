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

// ---- integration tests (need restic / sqlite3) ----

func need(t *testing.T, bin string) {
	t.Helper()
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not installed", bin)
	}
}

// testRunner builds a runner on temp dirs, including the scratch mount restic caches and stages restores
// through; the clock advances one hour per call so run timestamps differ.
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

// Full round trip for a sqlite source on the restic engine: backup stages consistent copies under
// .staging and commits them to the repository (which it then removes), verify runs integrity_check on
// the snapshot through the real decode path, and restore rewrites the volume even against stale
// sidecars — preserving the mode of the database it replaces.
func TestSQLiteBackupVerifyRestore(t *testing.T) {
	need(t, "sqlite3")
	requireRestic(t)
	ctx := t.Context()
	r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"db/app.db", "other.db"}}},
	})
	sq := func(p, q string) string {
		t.Helper()
		out, _, err := run(ctx, proc{env: os.Environ()}, "sqlite3", p, q)
		if err != nil {
			t.Fatalf("sqlite3 %s: %v", q, err)
		}
		return strings.TrimSpace(out)
	}
	mkdir(t, r.source, "db")
	db := filepath.Join(r.source, "db/app.db")
	other := filepath.Join(r.source, "other.db")
	sq(db, "create table t(v); insert into t values('one');")
	sq(other, "create table t(v); insert into t values('one');")

	var res jobcfg.Result
	if err := r.backup(ctx, &res); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("snapshot has %d files, want the 2 databases", res.Files)
	}
	if _, err := os.Stat(filepath.Join(r.policyDir, stagingDirName)); !os.IsNotExist(err) {
		t.Error("the staging tree survived the backup")
	}
	if fi, err := os.Stat(r.resticRepo()); err != nil || !fi.IsDir() {
		t.Errorf("restic repository missing: %v", err)
	}

	v := newVerifyRunner(t, r)
	v.cfg.Mode, v.cfg.Restore = jobcfg.ModeVerify, nil
	var vres jobcfg.Result
	if err := v.verify(ctx, &vres); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(vres.Message, "integrity_check") {
		t.Errorf("verify message = %q", vres.Message)
	}

	// The volume drifts, loses a database, and grows stale sidecars before the restore.
	sq(db, "insert into t values('two');")
	if err := os.Chmod(db, 0o600); err != nil {
		t.Fatal(err)
	}
	write(t, db+"-wal", "stale")
	write(t, db+"-shm", "stale")
	write(t, other+"-journal", "stale")
	os.Remove(other)

	r.cfg.Mode = jobcfg.ModeRestore
	r.cfg.Restore = &jobcfg.Restore{From: "latest"}
	if err := r.restore(ctx, &jobcfg.Result{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := sq(db, "select count(*) from t"); got != "1" {
		t.Errorf("count after restore = %q, want 1", got)
	}
	if got := sq(other, "select v from t"); got != "one" {
		t.Errorf("restored other.db = %q, want one", got)
	}
	for _, s := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(db + s); err == nil {
			t.Errorf("stale %s survived the restore", s)
		}
		if _, err := os.Stat(other + s); err == nil {
			t.Errorf("stale %s survived the restore", s)
		}
	}
	if info, _ := os.Stat(db); info.Mode().Perm() != 0o600 {
		t.Errorf("restored db mode = %v, want the replaced file's 0600", info.Mode())
	}

	r.cfg.Restore = &jobcfg.Restore{From: "latest", Paths: []string{"../x"}}
	if err := r.restore(ctx, &jobcfg.Result{}); err == nil {
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

// A symlink planted in a user-writable volume must not let a backup or restore touch anything outside
// it. The restic engine's containment lives in sourceRoot (files), captureSQLite (staging) and
// restoreSQLite (destination); each gate is exercised directly so this holds without a repository.
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
		if _, err := r.sourceRoot(); err == nil || !strings.Contains(err.Error(), "outside its volume") {
			t.Fatalf("symlinked subPath accepted: %v", err)
		}
	})

	t.Run("sqlite staging", func(t *testing.T) {
		r := testRunner(t, jobcfg.ModeBackup, v1alpha1.BackupPolicySpec{
			Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"link/secret.db"}}},
		})
		if err := os.Symlink(outside, filepath.Join(r.source, "link")); err != nil {
			t.Fatal(err)
		}
		stage, err := r.beginStaging()
		if err != nil {
			t.Fatal(err)
		}
		if err := r.captureSQLite(t.Context(), stage); err == nil || !strings.Contains(err.Error(), "outside its volume") {
			t.Fatalf("symlinked sqlite path accepted: %v", err)
		}
	})

	t.Run("restore destination", func(t *testing.T) {
		r := testRunner(t, jobcfg.ModeRestore, v1alpha1.BackupPolicySpec{
			Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"link/secret.db"}}},
		})
		r.cfg.Restore = &jobcfg.Restore{From: "latest"}
		if err := os.Symlink(outside, filepath.Join(r.source, "link")); err != nil {
			t.Fatal(err)
		}
		// Hand-build what stageSnapshot would have materialized from the snapshot.
		stage := mkdir(t, r.scratch, "restic-restore")
		write(t, filepath.Join(stage, "sqlite", "link", "secret.db"), "payload")
		if err := r.restoreSQLite(stage); err == nil {
			t.Fatal("restore through a symlink succeeded")
		}
		if b, _ := os.ReadFile(filepath.Join(outside, "secret.db")); string(b) == "payload" {
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

// A rollback journal left by the old database is "hot" for the restored file: SQLite would roll it back
// onto the restored data. It must be removed like -wal and -shm.
func TestSQLiteRestoreRemovesAllStaleSidecars(t *testing.T) {
	r := testRunner(t, jobcfg.ModeRestore, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"a.db"}}},
	})
	r.cfg.Restore = &jobcfg.Restore{From: "latest"}
	stage := mkdir(t, r.scratch, "restic-restore")
	write(t, filepath.Join(stage, "sqlite", "a.db"), "the committed database")
	db := filepath.Join(r.source, "a.db")
	write(t, db, "the drifted database")
	for _, s := range []string{"-wal", "-shm", "-journal"} {
		write(t, db+s, "stale")
	}
	if err := r.restoreSQLite(stage); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(db); string(b) != "the committed database" {
		t.Errorf("restored db = %q", b)
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
	r := testRunner(t, jobcfg.ModeRestore, v1alpha1.BackupPolicySpec{
		Source: v1alpha1.Source{Type: v1alpha1.SourceSQLite, SQLite: &v1alpha1.SQLiteSource{PVC: "x", Paths: []string{"a.db"}}},
	})
	r.cfg.Restore = &jobcfg.Restore{From: "latest"}
	stage := mkdir(t, r.scratch, "restic-restore")
	write(t, filepath.Join(stage, "sqlite", "a.db"), "the committed database")
	if err := os.Chown(r.source, 1234, 1234); err != nil {
		t.Fatal(err)
	}
	if err := r.restoreSQLite(stage); err != nil {
		t.Fatal(err)
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
