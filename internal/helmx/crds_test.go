package helmx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for n, b := range files {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func crd(name string) string {
	return "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + name + "\nspec: {}\n"
}

// A chart shaped like platform-crds: CRDs of its own plus a profile subchart.
func profileChart(t *testing.T) string {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"Chart.yaml":               "apiVersion: v2\nname: platform-crds\nversion: 0.2.0\ndependencies:\n  - name: extra\n    version: 0.1.0\n    condition: profiles.extra\n",
		"values.yaml":              "profiles:\n  extra: true\n",
		"crds/a.yaml":              "---\n" + crd("a.x.io"),
		"charts/extra/Chart.yaml":  "apiVersion: v2\nname: extra\nversion: 0.1.0\n",
		"charts/extra/crds/b.yaml": crd("b.x.io"), // no leading --- on purpose
		"platform-extra-off.yaml":  "profiles:\n  extra: false\n",
		"platform-extra-on.yaml":   "profiles:\n  extra: true\n",
	})
	return dir
}

func TestCRDBundleSelectsProfilesFromValues(t *testing.T) {
	dir := profileChart(t)
	c := New("", os.Stderr)
	off, err := c.CRDBundle(context.Background(), dir, "", filepath.Join(dir, "platform-extra-off.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := CountCRDKinds(off.Manifest); got != 1 || off.Union != 2 || off.Chart != "platform-crds 0.2.0" {
		t.Fatalf("profile off: %d CRDs, union %d, %q\n%s", got, off.Union, off.Chart, off.Manifest)
	}
	on, err := c.CRDBundle(context.Background(), dir, "", filepath.Join(dir, "platform-extra-on.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := CountCRDKinds(on.Manifest); got != 2 || !strings.Contains(string(on.Manifest), "b.x.io") {
		t.Fatalf("profile on: %d CRDs\n%s", got, on.Manifest)
	}
}

func TestCRDBundleWithoutValuesIsEveryCRD(t *testing.T) {
	dir := profileChart(t)
	b, err := New("", os.Stderr).CRDBundle(context.Background(), dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if CountCRDKinds(b.Manifest) != 2 || b.Union != 2 {
		t.Fatalf("%d / %d\n%s", CountCRDKinds(b.Manifest), b.Union, b.Manifest)
	}
	// a CRD file that lacks the leading --- must still be its own document
	if !strings.Contains(string(b.Manifest), "---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: b.x.io") {
		t.Fatalf("not separated:\n%s", b.Manifest)
	}
}

func TestCRDBundleRejectsValuesTheSchemaRefuses(t *testing.T) {
	dir := profileChart(t)
	writeFiles(t, dir, map[string]string{
		"values.schema.json": `{"type":"object","required":["profiles"],"properties":{"profiles":{"type":"object","required":["extra"],"additionalProperties":false,"properties":{"extra":{"type":"boolean"}}}}}`,
		"renamed.yaml":       "profiles:\n  extras: true\n",
	})
	_, err := New("", os.Stderr).CRDBundle(context.Background(), dir, "", filepath.Join(dir, "renamed.yaml"))
	if err == nil || !strings.Contains(err.Error(), "select CRD profiles") {
		t.Fatalf("a renamed profile key must fail, not drop CRDs: %v", err)
	}
}
