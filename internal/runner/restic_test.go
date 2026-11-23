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
