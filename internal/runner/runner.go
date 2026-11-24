// Package runner executes inside the mover pods: backup, verify and restore.
// It is configured entirely by jobcfg.Config and reports through the termination log.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

// EnvTerminationLog overrides jobcfg.TerminationLog (used by tests).
const EnvTerminationLog = "BS_TERMINATION_LOG"

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
	_, _ = fmt.Fprintf(r.out, "%s "+format+"\n", append([]any{r.now().UTC().Format(time.RFC3339)}, a...)...)
}

// Run reads the job config from the environment, executes it and returns the process exit code.
func Run(ctx context.Context) int {
	cfg, err := jobcfg.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "heirloom run:", err)
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
		fmt.Fprintln(os.Stderr, "heirloom: writing termination log:", err)
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
