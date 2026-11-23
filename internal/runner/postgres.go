package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const globalsFile = "globals.sql.gz"

// pgEnv is the environment for clients talking to the real server (PGPASSWORD is already set).
func (r *runner) pgEnv() []string {
	pg := r.cfg.Spec.Source.Postgres
	user, port := pg.User, pg.Port
	if user == "" {
		user = "postgres"
	}
	if port == 0 {
		port = 5432
	}
	return append(os.Environ(), "PGHOST="+pg.Host, "PGPORT="+strconv.Itoa(int(port)), "PGUSER="+user, "PGCONNECT_TIMEOUT=30")
}

// capturePostgres writes the per-database dumps and the globals dump under stage/postgres, the
// layout a restore reads back out of the snapshot. Committing them is the backup run's job.
func (r *runner) capturePostgres(ctx context.Context, stage string) error {
	pg := r.cfg.Spec.Source.Postgres
	if pg == nil {
		return fmt.Errorf("source.postgres is not set")
	}
	env := proc{env: r.pgEnv()}
	dbs := pg.Databases
	if len(dbs) == 0 {
		out, _, err := run(ctx, env, "psql", "-d", "postgres", "-tA", "-c",
			"SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY 1")
		if err != nil {
			return fmt.Errorf("listing databases: %w", err)
		}
		// One name per line: database names may contain spaces.
		for _, name := range strings.Split(out, "\n") {
			name = strings.TrimRight(name, "\r")
			if name != "" && !slices.Contains(pg.ExcludeDatabases, name) {
				dbs = append(dbs, name)
			}
		}
	}
	if len(dbs) == 0 {
		return fmt.Errorf("no databases to dump")
	}
	dir := filepath.Join(stage, "postgres")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, db := range dbs {
		if err := validDBName(db); err != nil {
			return err
		}
		r.logf("pg_dump %s", db)
		if err := dumpGzip(ctx, env, filepath.Join(dir, db+".sql.gz"), "pg_dump", "--create", "--clean", "--if-exists", "-d", db); err != nil {
			return fmt.Errorf("dump %s: %w", db, err)
		}
	}
	if pg.Globals != "skip" {
		args := []string{"--globals-only"}
		if pg.Globals == "no-role-passwords" {
			args = append(args, "--no-role-passwords")
		}
		r.logf("pg_dumpall %s", strings.Join(args, " "))
		if err := dumpGzip(ctx, env, filepath.Join(dir, globalsFile), "pg_dumpall", args...); err != nil {
			return fmt.Errorf("dump globals: %w", err)
		}
	}
	return nil
}

func validDBName(db string) error {
	if db == "" || db == "." || db == ".." || strings.ContainsAny(db, "/\x00") {
		return fmt.Errorf("invalid database name %q", db)
	}
	// globals.sql.gz holds the roles; a database of that name would be silently overwritten by it.
	if db == strings.TrimSuffix(globalsFile, ".sql.gz") {
		return fmt.Errorf("a database named %q cannot be backed up: its dump would collide with the roles dump; exclude or rename it", db)
	}
	return nil
}

// dumpGzip streams a command's stdout through gzip into path (mode 0600) and fsyncs it.
// Failure of the command (pipefail) or empty output is an error.
func dumpGzip(ctx context.Context, p proc, path, name string, args ...string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := command(ctx, p, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	n, copyErr := io.Copy(zw, pipe)
	if copyErr != nil {
		// The destination failed (for example the NAS is full). Stop the dump instead of leaving Wait blocked on a
		// pipe nobody reads, which would hang until the Job deadline.
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if copyErr != nil {
		return fmt.Errorf("writing %s output: %w", name, copyErr)
	}
	if waitErr != nil {
		return fmt.Errorf("%s: %w: %s", name, waitErr, tail(strings.TrimSpace(stderr.String()), 1000))
	}
	if n == 0 {
		return fmt.Errorf("%s produced no output", name)
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// loadDump pipes a gzipped SQL file into psql. Strict mode stops at the first error; tolerant mode
// (used for globals) ignores "already exists" errors.
func loadDump(ctx context.Context, env []string, file string, tolerant bool) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(file), err)
	}
	// Dumps carry DROP/CREATE DATABASE, so connect to a database other than the one being replaced.
	args := []string{"-d", maintenanceDB(filepath.Base(file)), "-f", "-"}
	if !tolerant {
		args = append([]string{"-v", "ON_ERROR_STOP=1"}, args...)
	}
	_, stderr, err := run(ctx, proc{env: env, stdin: zr}, "psql", args...)
	if err != nil && !tolerant {
		return fmt.Errorf("loading %s: %w", filepath.Base(file), err)
	}
	if tolerant {
		var bad []string
		for _, l := range strings.Split(stderr, "\n") {
			if strings.Contains(l, "ERROR:") && !strings.Contains(l, "already exists") {
				bad = append(bad, strings.TrimSpace(l))
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("loading %s: %s", filepath.Base(file), tail(strings.Join(bad, "; "), 1000))
		}
	}
	return nil
}

// maintenanceDB is the database psql connects to while loading the named dump file. The postgres
// database itself cannot be dropped while connected to it, so its dump is loaded via template1.
func maintenanceDB(dumpFile string) string {
	if dumpFile == "postgres.sql.gz" {
		return "template1"
	}
	return "postgres"
}

// loadGlobals replays the roles dump when the snapshot has one. Policies with globals: skip have none.
func loadGlobals(ctx context.Context, env []string, pgdir string) error {
	file := filepath.Join(pgdir, globalsFile)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		return nil
	}
	return loadDump(ctx, env, file, true)
}

// dumpDatabases lists the database dump names in a snapshot's postgres dir.
func dumpDatabases(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var dbs []string
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".sql.gz"); ok && e.Name() != globalsFile {
			dbs = append(dbs, n)
		}
	}
	return dbs, nil
}
