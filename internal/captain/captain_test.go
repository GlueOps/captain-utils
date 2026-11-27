package captain

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func writeRepo(t *testing.T, files map[string]string) Repo {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Repo{Dir: dir}
}

func TestReadVersionsKeepsScalarsAsWritten(t *testing.T) {
	r := writeRepo(t, map[string]string{"VERSIONS/aws.yaml": `versions:
  - name: kubernetes_version
    version: 1.30
  - name: ami_release_version
    version: 1.34.9-20260801
  - name: quoted
    version: "1.34"
`})
	v, err := r.AWSVersions()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"kubernetes_version": "1.30", "ami_release_version": "1.34.9-20260801", "quoted": "1.34"} {
		if got, _ := v.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestReadVersionsTrimsTrailingSpaceInName(t *testing.T) {
	// nonprod.venus has "glueops_platform_helm_chart_version " with a trailing space
	r := writeRepo(t, map[string]string{"VERSIONS/glueops.yaml": "versions:\n  - name: glueops_platform_helm_chart_version \n    version: v0.85.0\n"})
	v, err := r.GlueOpsVersions()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v.Require(PlatformChartVersion); err != nil || got != "v0.85.0" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Repo{Dir: t.TempDir()}.GlueOpsVersions()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}

func TestPlatformCRDsPin(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string // empty: an error
	}{
		"tag":        {"versions:\n  - name: platform_crds_version\n    version: v0.1.6\n", "v0.1.6"},
		"no v":       {"versions:\n  - name: platform_crds_version\n    version: 0.1.6\n", "0.1.6"},
		"branch":     {"versions:\n  - name: platform_crds_version\n    version: main\n", ""},
		"missing":    {"versions:\n  - name: codespace_version\n    version: v1.0.0\n", ""},
		"duplicated": {"versions:\n  - name: platform_crds_version\n    version: v0.1.6\n  - name: platform_crds_version\n    version: v0.1.7\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := writeRepo(t, map[string]string{"VERSIONS/glueops.yaml": tc.body})
			got, err := r.PlatformCRDsPin()
			if (err == nil) != (tc.want != "") || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := (Repo{Dir: t.TempDir()}).PlatformCRDsPin(); err == nil {
		t.Fatal("no VERSIONS file must be an error")
	}
}
