package runner

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
)

const verifySampleFiles = 100

func (r *runner) verify(ctx context.Context, res *jobcfg.Result) error {
	from := "latest"
	if r.cfg.Restore != nil {
		from = r.cfg.Restore.From
	}
	name, err := selectSnapshot(r.policyDir, from)
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
	r.logf("verifying snapshot %s", name)

	switch r.cfg.Spec.Source.Type {
	case v1alpha1.SourceFiles:
		err = r.verifyFiles(dir, st.Files, st.Bytes)
	case v1alpha1.SourceSQLite:
		err = r.verifySQLite(ctx, dir)
	case v1alpha1.SourcePostgres:
		err = r.verifyPostgres(ctx, dir)
	default:
		err = fmt.Errorf("unknown source type %q", r.cfg.Spec.Source.Type)
	}
	if err != nil {
		return fmt.Errorf("verify %s: %w", name, err)
	}
	res.Message = "verified snapshot " + name
	return nil
}

func (r *runner) verifyFiles(dir string, wantFiles, wantBytes int64) error {
	data := filepath.Join(dir, "data")
	files, bytes, err := countTree(data)
	if err != nil {
		return err
	}
	if files != wantFiles || bytes != wantBytes {
		return fmt.Errorf("snapshot has %d files/%d bytes, status says %d/%d", files, bytes, wantFiles, wantBytes)
	}
	// Reservoir-sample regular files and read each one completely.
	var sample []string
	seen := 0
	err = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		seen++
		if len(sample) < verifySampleFiles {
			sample = append(sample, p)
		} else if j := rand.IntN(seen); j < verifySampleFiles {
			sample[j] = p
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range sample {
		f, err := os.Open(p)
		if err == nil {
			_, err = io.Copy(io.Discard, f)
			f.Close()
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", p, err)
		}
	}
	r.logf("%d files, %d bytes match status; read %d sampled files", files, bytes, len(sample))
	return nil
}

func (r *runner) verifySQLite(ctx context.Context, dir string) error {
	root := filepath.Join(dir, "sqlite")
	var failures []string
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		n++
		if err := sqliteCheck(ctx, p); err != nil {
			rel, _ := filepath.Rel(root, p)
			failures = append(failures, rel+": "+err.Error())
		}
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("snapshot contains no sqlite databases")
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	r.logf("%d sqlite databases pass integrity_check", n)
	return nil
}

// pgBinary finds a server binary on PATH or in the Debian-style /usr/lib/postgresql/*/bin.
func pgBinary(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	m, _ := filepath.Glob("/usr/lib/postgresql/*/bin/" + name)
	if len(m) > 0 {
		return m[len(m)-1], nil
	}
	return "", fmt.Errorf("%s not found in the image", name)
}

func (r *runner) verifyPostgres(ctx context.Context, dir string) (err error) {
	pg := r.cfg.Spec.Source.Postgres
	if pg == nil {
		return fmt.Errorf("source.postgres is not set")
	}
	pgdir := filepath.Join(dir, "postgres")
	dbs, err := dumpDatabases(pgdir)
	if err != nil {
		return err
	}
	if len(dbs) == 0 {
		return fmt.Errorf("snapshot contains no database dumps")
	}
	initdb, err := pgBinary("initdb")
	if err != nil {
		return err
	}
	postgres, err := pgBinary("postgres")
	if err != nil {
		return err
	}

	uid := int(pg.UID)
	if uid == 0 {
		uid = 999
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}
	sock := filepath.Join(r.scratch, "sock")
	data := filepath.Join(r.scratch, "data")
	for _, d := range []string{sock, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	for _, d := range []string{r.scratch, sock, data} {
		if err := os.Chown(d, uid, uid); err != nil {
			return fmt.Errorf("chown scratch: %w", err)
		}
	}
	asPG := proc{env: append(os.Environ(), "HOME="+r.scratch), cred: cred}
	r.logf("initdb in %s", data)
	if _, _, err := run(ctx, asPG, initdb, "-D", data, "-U", "postgres", "--auth=trust"); err != nil {
		return err
	}

	srv := command(ctx, asPG, postgres, "-D", data, "-c", "listen_addresses=", "-c", "unix_socket_directories="+sock,
		"-c", "fsync=off", "-c", "full_page_writes=off", "-c", "synchronous_commit=off")
	srv.Stdout, srv.Stderr = os.Stdout, os.Stderr
	if err := srv.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	defer func() { // fast shutdown, then wait; never block forever (signalling uid 999 as root needs CAP_KILL)
		if err := srv.Process.Signal(syscall.SIGINT); err != nil {
			r.logf("stopping scratch postgres: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(60 * time.Second):
			srv.Process.Kill()
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				r.logf("scratch postgres did not stop; exiting anyway")
			}
		}
	}()

	env := append(os.Environ(), "PGHOST="+sock, "PGUSER=postgres", "PGPORT=5432")
	ready := false
	for i := 0; i < 120 && !ready; i++ {
		select {
		case e := <-exited:
			exited <- e
			return fmt.Errorf("scratch postgres exited early: %v", e)
		default:
		}
		if _, _, err := run(ctx, proc{env: env}, "pg_isready", "-q"); err == nil {
			ready = true
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !ready {
		return fmt.Errorf("scratch postgres did not become ready")
	}

	if err := loadGlobals(ctx, env, pgdir); err != nil {
		return err
	}
	var failures []string
	for _, db := range dbs {
		r.logf("restoring %s", db)
		if err := loadDump(ctx, env, filepath.Join(pgdir, db+".sql.gz"), false); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	queries := verifySQL(r.cfg.Spec.Verify)
	target := ""
	if len(queries) > 0 {
		var err error
		if target, err = verifyDatabase(r.cfg.Spec, dbs); err != nil {
			return err
		}
	}
	for _, q := range queries {
		out, _, err := run(ctx, proc{env: env}, "psql", "-tA", "-d", target, "-c", q)
		got := strings.TrimSpace(out)
		if err != nil {
			got = err.Error()
		}
		if err != nil || got != "t" {
			failures = append(failures, fmt.Sprintf("assertion %q on database %q returned %q", q, target, tail(got, 200)))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	r.logf("%d databases restored, %d assertions passed", len(dbs), len(verifySQL(r.cfg.Spec.Verify)))
	return nil
}

// verifyDatabase picks the database the SQL assertions run against: verify.database, else the single
// database the policy dumps. It never guesses between several.
func verifyDatabase(spec v1alpha1.BackupPolicySpec, restored []string) (string, error) {
	name := ""
	if spec.Verify != nil {
		name = spec.Verify.Database
	}
	if name == "" {
		if pg := spec.Source.Postgres; pg != nil && len(pg.Databases) == 1 {
			name = pg.Databases[0]
		}
	}
	if name == "" {
		return "", fmt.Errorf("verify.sql needs verify.database when the policy dumps more than one database")
	}
	for _, d := range restored {
		if d == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("verify.database %q is not in the snapshot (has %s)", name, strings.Join(restored, ", "))
}

func verifySQL(v *v1alpha1.Verify) []string {
	if v == nil {
		return nil
	}
	return v.SQL
}
