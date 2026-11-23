package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willuhmjs/backshots/api/v1alpha1"
	"github.com/willuhmjs/backshots/internal/jobcfg"
	"github.com/willuhmjs/backshots/internal/snapshot"
)

// requireRestic skips the restic engine tests on machines without the binary; CI images ship it.
func requireRestic(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
}

// newResticRunner points a restic-engine runner at temp dirs, like the other runner tests.
func newResticRunner(t *testing.T, ret snapshot.Retention) (*runner, string) {
	t.Helper()
	requireRestic(t)
	root := t.TempDir()
	src := mkdir(t, root, "source")
	spec := v1alpha1.BackupPolicySpec{
		Engine: v1alpha1.EngineRestic,
		// The API server defaults this to 50; a fake config must carry the same value the runner would see.
		MinSourcePercent: 50,
		Source:           v1alpha1.Source{Type: v1alpha1.SourceFiles, Files: &v1alpha1.FilesSource{PVC: "p"}},
	}
	r := newRunner(jobcfg.Config{Mode: jobcfg.ModeBackup, Namespace: "ns", Policy: "p", Spec: spec, Retention: ret})
	r.policyDir = mkdir(t, root, "store")
	r.source = src
	r.scratch = mkdir(t, root, "scratch")
	r.out = new(bytes.Buffer)
	return r, src
}

// dirBytes is the disk size of a tree, used to measure what a run really added to the repository.
func dirBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

// TestResticDedup is the proof of the engine: change ONE BYTE in a 20 MiB random file and the repository
// must grow by far less than 20 MiB. The plain engine would hardlink but still needs the bytes; this must
// stay well under the whole file even after restic rewrites the pack that holds the changed chunk.
func TestResticDedup(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	ctx := context.Background()
	big := make([]byte, 20<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "small.txt"), "hello\n")

	var res1 jobcfg.Result
	if err := r.backup(ctx, &res1); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if res1.Snapshot == "" || res1.Files != 2 {
		t.Errorf("res = %+v", res1)
	}
	after1 := dirBytes(t, r.resticRepo())

	f, err := os.OpenFile(filepath.Join(src, "big.bin"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, 12345); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var res2 jobcfg.Result
	if err := r.backup(ctx, &res2); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	grew := dirBytes(t, r.resticRepo()) - after1
	t.Logf("repo %s after backup 1, grew %s after one-byte change", humanBytes(after1), humanBytes(grew))
	if grew >= 10<<20 {
		t.Errorf("repository grew by %s after a one-byte change; dedup is not working", humanBytes(grew))
	}
	if !strings.Contains(res2.Message, "added to repository") {
		t.Errorf("message lacks the dedup result: %s", res2.Message)
	}
}

// The retention counts must reach restic forget: three snapshots at once with keep-hourly 1 leaves one.
func TestResticRetention(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 1})
	ctx := context.Background()
	write(t, filepath.Join(src, "a.txt"), "one\n")
	for i := 0; i < 3; i++ {
		write(t, filepath.Join(src, "a.txt"), string(rune('a'+i))+"\n")
		var res jobcfg.Result
		if err := r.backup(ctx, &res); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		if i > 0 && res.Pruned != 1 {
			t.Errorf("backup %d pruned %d, want 1 (kept %d)", i, res.Pruned, res.Kept)
		}
	}
	out, err := r.resticRun(ctx, "", "snapshots", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var snaps []resticSnapshot
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Errorf("%d snapshots remain, want 1", len(snaps))
	}
}

// The empty/shrunken-source guard applies to this engine too: an emptied volume must not replace a good
// history, and an empty first source must not create an empty snapshot.
func TestResticGuard(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	ctx := context.Background()

	var res jobcfg.Result
	// restic refuses an empty backup itself; either way no snapshot may exist and the run must fail.
	if err := r.backup(ctx, &res); err == nil {
		t.Fatal("empty first source must not produce a snapshot")
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		write(t, filepath.Join(src, n+".txt"), n)
	}
	if err := r.backup(ctx, &res); err != nil {
		t.Fatalf("first real backup: %v", err)
	}
	for _, n := range []string{"b", "c", "d"} {
		if err := os.Remove(filepath.Join(src, n+".txt")); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.backup(ctx, &res); err == nil {
		t.Fatal("1 file after a snapshot of 4 must trip the guard")
	} else if _, ok := err.(guardError); !ok {
		t.Errorf("want guardError, got %T: %v", err, err)
	}
}

// files.exclude entries must stay out of the snapshot.
func TestResticExclude(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	r.cfg.Spec.Source.Files.Exclude = []string{"*.tmp"}
	write(t, filepath.Join(src, "keep.txt"), "k")
	write(t, filepath.Join(src, "drop.tmp"), "d")
	var res jobcfg.Result
	if err := r.backup(context.Background(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Errorf("snapshot has %d files, want 1 (exclude not applied?)", res.Files)
	}
}

// subPath is contained and backed up as the snapshot root.
func TestResticSubPath(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	r.cfg.Spec.Source.Files.SubPath = "inner"
	mkdir(t, src, "inner")
	write(t, filepath.Join(src, "inner", "x.txt"), "x")
	write(t, filepath.Join(src, "out.txt"), "outside")
	var res jobcfg.Result
	if err := r.backup(context.Background(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Errorf("snapshot has %d files, want 1 (sub only)", res.Files)
	}
}

// treeState fingerprints a tree: per path the permission bits and the content hash.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			m[rel] = "dir:" + info.Mode().Perm().String()
			return nil
		}
		if !d.Type().IsRegular() {
			t.Fatalf("test tree holds unexpected entry type: %s", d.Type())
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		m[rel] = info.Mode().Perm().String() + ":" + sha256Hex(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sameTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A backup followed by a restore by id must put the volume back exactly as it was, modes included.
func TestResticRestoreRoundTrip(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	ctx := context.Background()
	write(t, filepath.Join(src, "top.txt"), "hello\n")
	write(t, filepath.Join(src, "sub", "nested.bin"), "\x00\x01binary\xff")
	write(t, filepath.Join(src, "sub", "deep", "x.txt"), "deep\n")
	if err := os.Chmod(filepath.Join(src, "top.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, src)

	var res jobcfg.Result
	if err := r.backup(ctx, &res); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(src); err != nil {
		t.Fatal(err)
	}
	mkdir(t, src)

	r.cfg.Mode = jobcfg.ModeRestore
	r.cfg.Restore = &jobcfg.Restore{From: res.Snapshot} // the short id, the way status shows it
	var rres jobcfg.Result
	if err := r.restore(ctx, &rres); err != nil {
		t.Fatal(err)
	}
	if got := treeState(t, src); !sameTrees(before, got) {
		t.Errorf("restored tree differs:\nwant %v\ngot  %v", before, got)
	}
}

// delete must remove what is not in the snapshot and never remove paths the backup excluded — the same
// invariants the files engine has. (delete together with paths is rejected by the controller: restic
// cannot honour exclude while restricting include patterns.)
func TestResticRestoreDelete(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	r.cfg.Spec.Source.Files.Exclude = []string{"*.tmp"}
	ctx := context.Background()
	write(t, filepath.Join(src, "keep", "a.txt"), "original\n")
	write(t, filepath.Join(src, "keep", "cache.tmp"), "excluded\n")
	if err := r.backup(ctx, &jobcfg.Result{}); err != nil {
		t.Fatal(err)
	}
	// Source moved on: a.txt changed, a stale file next to it, and new files inside and outside the tree.
	write(t, filepath.Join(src, "keep", "a.txt"), "changed\n")
	write(t, filepath.Join(src, "keep", "gone.txt"), "stale\n")
	write(t, filepath.Join(src, "keep", "cache.tmp"), "still mine\n")
	write(t, filepath.Join(src, "added.txt"), "stale\n")
	write(t, filepath.Join(src, "junk.tmp"), "excluded\n")

	r.cfg.Mode = jobcfg.ModeRestore
	r.cfg.Restore = &jobcfg.Restore{From: "latest", Delete: true}
	if err := r.restore(ctx, &jobcfg.Result{}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(src, "keep", "a.txt")); err != nil || string(b) != "original\n" {
		t.Errorf("keep/a.txt = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(src, "keep", "gone.txt")); !os.IsNotExist(err) {
		t.Error("keep/gone.txt is not in the snapshot and must be deleted")
	}
	if _, err := os.Stat(filepath.Join(src, "added.txt")); !os.IsNotExist(err) {
		t.Error("added.txt is not in the snapshot and must be deleted")
	}
	for _, keep := range []string{filepath.Join(src, "keep", "cache.tmp"), filepath.Join(src, "junk.tmp")} {
		if b, err := os.ReadFile(keep); err != nil || string(b) == "" {
			t.Errorf("%s is excluded from backup and must survive --delete (%q, %v)", keep, b, err)
		}
	}
}

// A restricted restore (paths without delete) restores exactly those paths and touches nothing else.
func TestResticRestorePaths(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	ctx := context.Background()
	write(t, filepath.Join(src, "keep", "a.txt"), "original\n")
	write(t, filepath.Join(src, "root.txt"), "original\n")
	if err := r.backup(ctx, &jobcfg.Result{}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(src, "keep", "a.txt"), "changed\n")
	write(t, filepath.Join(src, "root.txt"), "changed\n")
	write(t, filepath.Join(src, "keep", "gone.txt"), "stale\n")

	r.cfg.Mode = jobcfg.ModeRestore
	r.cfg.Restore = &jobcfg.Restore{From: "latest", Paths: []string{"keep"}}
	if err := r.restore(ctx, &jobcfg.Result{}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(src, "keep", "a.txt")); err != nil || string(b) != "original\n" {
		t.Errorf("keep/a.txt = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(src, "root.txt")); err != nil || string(b) != "changed\n" {
		t.Errorf("root.txt must be untouched by a restricted restore: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(src, "keep", "gone.txt")); err != nil {
		t.Error("without delete, keep/gone.txt must survive")
	}
}

// newVerifyRunner re-points a restic runner at the same store with a fresh scratch, as a verify pod has
// (a fresh scratch also means a cold restic cache: no stale index can mask corruption).
func newVerifyRunner(t *testing.T, r *runner) *runner {
	t.Helper()
	v := newRunner(r.cfg)
	v.policyDir, v.source, v.out = r.policyDir, r.source, new(bytes.Buffer)
	s, err := os.MkdirTemp(filepath.Dir(r.scratch), "verify-scratch-")
	if err != nil {
		t.Fatal(err)
	}
	v.scratch = s
	return v
}

func TestResticVerify(t *testing.T) {
	r, src := newResticRunner(t, snapshot.Retention{Hourly: 24})
	write(t, filepath.Join(src, "a.txt"), "aaa\n")
	write(t, filepath.Join(src, "sub"), "x")
	var res jobcfg.Result
	if err := r.backup(context.Background(), &res); err != nil {
		t.Fatal(err)
	}

	v := newVerifyRunner(t, r)
	v.cfg.Mode = jobcfg.ModeVerify
	v.cfg.Restore = nil
	var vres jobcfg.Result
	if err := v.verify(context.Background(), &vres); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if vres.Snapshot != res.Snapshot || !strings.Contains(vres.Message, "check ok") {
		t.Errorf("verify result = %+v", vres)
	}

	// Destroy a pack file (restic packs are stored read-only; this is bit rot or a rogue NAS user). A fresh
	// scratch, as a real verify pod has, means no stale index can mask it.
	repo := r.resticRepo()
	var packs []string
	filepath.WalkDir(filepath.Join(repo, "data"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			packs = append(packs, p)
		}
		return nil
	})
	if len(packs) == 0 {
		t.Fatal("no pack files")
	}
	pack := packs[len(packs)-1]
	info, err := os.Stat(pack)
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, info.Size())
	if _, err := rand.Read(junk); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pack, 0o600); err != nil { // restic stores packs read-only
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, junk, 0o644); err != nil {
		t.Fatal(err)
	}

	v2 := newVerifyRunner(t, r)
	v2.cfg.Mode = jobcfg.ModeVerify
	if err := v2.verify(context.Background(), &jobcfg.Result{}); err == nil {
		t.Fatal("verify must fail on a corrupted repository")
	}
}
