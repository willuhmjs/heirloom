package crds

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// The image can only install what it embeds, and Helm's hook only upgrades what the image carries:
// a missing, unparseable, or drifted manifest silently strands a cluster on an old schema.
func TestEmbeddedManifests(t *testing.T) {
	entries, err := FS.ReadDir("manifests")
	if err != nil {
		t.Fatalf("reading embedded manifests: %v", err)
	}
	want := map[string]bool{"backuppolicies.heirloom.io": false, "backupstores.heirloom.io": false, "restores.heirloom.io": false}
	if len(entries) != len(want) {
		t.Fatalf("embedded %d manifests, want %d", len(entries), len(want))
	}
	for _, e := range entries {
		raw, err := FS.ReadFile("manifests/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if doc.APIVersion != "apiextensions.k8s.io/v1" || doc.Kind != "CustomResourceDefinition" {
			t.Errorf("%s: %s/%s, want apiextensions.k8s.io/v1 CustomResourceDefinition", e.Name(), doc.APIVersion, doc.Kind)
		}
		if _, ok := want[doc.Metadata.Name]; !ok {
			t.Errorf("%s: unexpected CRD %q", e.Name(), doc.Metadata.Name)
			continue
		}
		want[doc.Metadata.Name] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("CRD %s is not embedded", name)
		}
	}
}

// internal/crds/manifests is the copy the image embeds; the Makefile syncs it from config/crd.
// If this fails, run `make manifests`.
func TestEmbeddedMatchesGenerated(t *testing.T) {
	src, err := filepath.Abs("../../config/crd")
	if err != nil {
		t.Fatal(err)
	}
	outs, err := os.ReadDir(src)
	if err != nil {
		t.Skipf("config/crd not readable (%v): source checkout without generated files?", err)
	}
	if len(outs) == 0 {
		t.Skip("config/crd is empty")
	}
	for _, o := range outs {
		if o.IsDir() || filepath.Ext(o.Name()) != ".yaml" {
			continue
		}
		got, err := FS.ReadFile("manifests/" + o.Name())
		if err != nil {
			t.Fatalf("manifests/%s is missing from the embed: %v", o.Name(), err)
		}
		want, err := os.ReadFile(filepath.Join(src, o.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("manifests/%s differs from config/crd/%s: run `make manifests`", o.Name(), o.Name())
		}
	}
}
