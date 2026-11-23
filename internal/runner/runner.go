// Package runner executes inside the mover pods: backup, verify and restore.
// It is configured entirely by jobcfg.Config and reports through the termination log.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/snapshot"
)

// EnvTerminationLog overrides jobcfg.TerminationLog (used by tests).
const EnvTerminationLog = "BS_TERMINATION_LOG"

const incompleteMaxAge = 24 * time.Hour

// runner holds the config plus the filesystem locations, which tests point at temp dirs.
type runner struct {
	cfg       jobcfg.Config
	policyDir string
	source    string
	scratch   string
	now       func() time.Time
	out       io.Writer
}

func newRunner(cfg jobcfg.Config) *runner {
	return &runner{
		cfg:       cfg,
		policyDir: cfg.PolicyDir(),
		source:    jobcfg.SourceMount,
		scratch:   jobcfg.ScratchMount,
		now:       time.Now,
		out:       os.Stdout,
	}
}

func (r *runner) logf(format string, a ...any) {
	fmt.Fprintf(r.out, "%s "+format+"\n", append([]any{r.now().UTC().Format(time.RFC3339)}, a...)...)
}

// Run reads the job config from the environment, executes it and returns the process exit code.
func Run(ctx context.Context) int {
	cfg, err := jobcfg.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "backshots run:", err)
		writeResult(termLogPath(), jobcfg.Result{Mode: cfg.Mode, Message: err.Error(), Time: time.Now()})
		return 2
	}
	res := newRunner(cfg).execute(ctx)
	writeResult(termLogPath(), res)
	if !res.OK {
		return 1
	}
	return 0
}

func termLogPath() string {
	if p := os.Getenv(EnvTerminationLog); p != "" {
		return p
	}
	return jobcfg.TerminationLog
}

// execute dispatches on the mode and always returns a Result (never an error).
func (r *runner) execute(ctx context.Context) jobcfg.Result {
	start := r.now()
	res := jobcfg.Result{Mode: r.cfg.Mode, Time: start}
	var err error
	switch r.cfg.Mode {
	case jobcfg.ModeBackup:
		err = r.backup(ctx, &res)
	case jobcfg.ModeVerify:
		err = r.verify(ctx, &res)
	case jobcfg.ModeRestore:
		err = r.restore(ctx, &res)
	default:
		err = fmt.Errorf("unknown mode %q", r.cfg.Mode)
	}
	res.Duration = r.now().Sub(start).Seconds()
	res.OK = err == nil
	if err != nil {
		res.Message = err.Error()
		r.logf("FAILED: %v", err)
	} else {
		r.logf("OK: %s", res.Message)
	}
	return res
}

// writeResult stores the result as JSON; the kubelet limits the termination message to 4096 bytes.
func writeResult(path string, res jobcfg.Result) {
	if len(res.Message) > 3000 {
		res.Message = res.Message[:1500] + "\n...[truncated]...\n" + res.Message[len(res.Message)-1400:]
	}
	b, err := json.Marshal(res)
	if err == nil {
		err = os.WriteFile(path, b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "backshots: writing termination log:", err)
	}
}

// ---- command helpers ----

// proc describes optional settings for exec.
type proc struct {
	env   []string
	stdin io.Reader
	cred  *syscall.Credential
	// dir is the working directory the command starts in.
	dir string
}

// execCommand is the single place external binaries are started.
var execCommand = exec.CommandContext

func command(ctx context.Context, p proc, name string, args ...string) *exec.Cmd {
	cmd := execCommand(ctx, name, args...)
	cmd.Env = p.env
	cmd.Stdin = p.stdin
	cmd.Dir = p.dir
	if p.cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: p.cred}
	}
	return cmd
}

// run executes a command to completion and returns stdout and stderr; a non-zero exit is an error
// that includes the tail of stderr.
func run(ctx context.Context, p proc, name string, args ...string) (string, string, error) {
	var o, e bytes.Buffer
	cmd := command(ctx, p, name, args...)
	cmd.Stdout, cmd.Stderr = &o, &e
	if err := cmd.Run(); err != nil {
		return o.String(), e.String(), fmt.Errorf("%s: %w: %s", name, err, tail(strings.TrimSpace(e.String()+" "+o.String()), 1000))
	}
	return o.String(), e.String(), nil
}

func tail(s string, n int) string {
	s = redact(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// sqlDataLine matches the lines in which PostgreSQL quotes data: the offending key or row (DETAIL), the surrounding
// statement or function (CONTEXT, STATEMENT, QUERY). They would otherwise travel into Job and policy status, which
// anyone who can read the policy can see. The primary ERROR line is kept: it is what makes a failure diagnosable.
var sqlDataLine = regexp.MustCompile(`^\s*(?:psql:\S+\s+)?(?:DETAIL|CONTEXT|STATEMENT|QUERY):`)

// redact drops the lines of s that quote data.
func redact(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !sqlDataLine.MatchString(l) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// ---- paths and snapshots ----

// cleanRel validates a user-supplied path relative to a volume/snapshot root and rejects traversal.
func cleanRel(p string) (string, error) {
	p = strings.TrimLeft(p, "/")
	if p == "" || p == "." {
		return "", fmt.Errorf("empty path")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("path %q must not contain '..'", p)
		}
	}
	return filepath.Clean(p), nil
}

// resolveWithin joins a cleaned relative path onto root, follows every symlink on the way, and fails if the
// result leaves root. Source volumes are written by workloads, so a symlink planted there must not make the
// root mover read or overwrite anything outside the volume. The path itself need not exist (restore targets):
// the longest existing prefix is resolved and the remainder appended.
func resolveWithin(root, rel string) (string, error) {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(rootReal, rel)
	existing, rest := full, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	if real != rootReal && !strings.HasPrefix(real, rootReal+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside its volume (symlink?)", rel)
	}
	return filepath.Join(real, rest), nil
}

// listSnapshots returns the complete snapshot names in policyDir, oldest first. A missing dir is empty.
func listSnapshots(policyDir string) ([]string, error) {
	ents, err := os.ReadDir(policyDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return snapshot.Sorted(names), nil
}

// selectSnapshot resolves "latest"/"" or an exact snapshot name to a complete snapshot.
func selectSnapshot(policyDir, from string) (string, error) {
	names, err := listSnapshots(policyDir)
	if err != nil {
		return "", err
	}
	if from == "" || from == "latest" {
		if len(names) == 0 {
			return "", fmt.Errorf("no complete snapshots in %s", policyDir)
		}
		return names[len(names)-1], nil
	}
	if _, ok := snapshot.Parse(from); !ok { // also rejects any path separators or '..'
		return "", fmt.Errorf("invalid snapshot name %q", from)
	}
	for _, n := range names {
		if n == from {
			return n, nil
		}
	}
	return "", fmt.Errorf("snapshot %q not found (or not complete)", from)
}

func readStatus(snapDir string) (snapshot.Status, error) {
	var st snapshot.Status
	b, err := os.ReadFile(filepath.Join(snapDir, snapshot.StatusFile))
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func writeStatus(dir string, st snapshot.Status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dir, snapshot.StatusFile), b, 0o644)
}

func writeFileSync(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// countTree counts non-directory entries and the bytes of regular files under root.
func countTree(root string) (files, bytes int64, err error) {
	err = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files++
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	return
}

// cleanIncomplete removes .incomplete-* directories whose mtime is older than maxAge.
func cleanIncomplete(policyDir string, now time.Time, maxAge time.Duration) (removed []string, err error) {
	ents, err := os.ReadDir(policyDir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || !snapshot.IsIncomplete(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(policyDir, e.Name())); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}

// prune deletes snapshots that retention no longer keeps and returns (kept, pruned).
func prune(policyDir string, ret snapshot.Retention) (int, int, error) {
	if ret.IsZero() {
		ret = snapshot.Default
	}
	names, err := listSnapshots(policyDir)
	if err != nil {
		return 0, 0, err
	}
	// Finish deletions an earlier, interrupted run started.
	if err := sweepDeleting(policyDir); err != nil {
		return len(names), 0, err
	}
	del := snapshot.Prune(names, ret)
	var firstErr error
	removed := 0
	for _, n := range del {
		// Rename first: it is atomic, so a half-deleted tree never keeps a snapshot name that restore or
		// verify could pick. A crash mid-delete leaves a .deleting- directory that the next run finishes.
		tomb := filepath.Join(policyDir, snapshot.DeletingPrefix+n)
		if err := os.Rename(filepath.Join(policyDir, n), tomb); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
		if err := os.RemoveAll(tomb); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(names) - removed, removed, firstErr
}

// sweepDeleting removes .deleting- directories left behind by an interrupted prune.
func sweepDeleting(policyDir string) error {
	ents, err := os.ReadDir(policyDir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), snapshot.DeletingPrefix) {
			if err := os.RemoveAll(filepath.Join(policyDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
