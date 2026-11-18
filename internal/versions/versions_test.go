package versions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glueops/captain-utils/internal/captain"
)

type fakeCluster struct {
	server   string
	releases map[string]string
	labels   map[string]string
	images   map[string]string
	fail     bool
}

var errDown = errors.New("connection refused")

func (f fakeCluster) ServerVersion(context.Context) (string, error) {
	if f.fail {
		return "", errDown
	}
	return f.server, nil
}
func (f fakeCluster) ReleaseChartVersion(_ context.Context, r string) (string, error) {
	if f.fail {
		return "", errDown
	}
	return f.releases[r], nil
}
func (f fakeCluster) DeploymentLabel(_ context.Context, ns, n, k string) (string, error) {
	if f.fail {
		return "", errDown
	}
	return f.labels[ns+"/"+n+"/"+k], nil
}
func (f fakeCluster) DeploymentImage(_ context.Context, ns, n string) (string, error) {
	if f.fail {
		return "", errDown
	}
	return f.images[ns+"/"+n], nil
}

const glueopsYAML = `versions:
  - name: glueops_platform_helm_chart_version
    version: v0.85.0
  - name: argocd_helm_chart_version
    version: 9.3.7
  - name: argocd_app_version
    version: v3.2.12
  - name: calico_helm_chart_version
    version: v3.31.4
  - name: calico_ctl_version
    version: v3.31.4
  - name: tigera_operator_version
    version: v1.40.7
  - name: terraform_module_version
    version: v0.51.0
`

const awsYAML = `versions:
  - name: csi_driver_version
    version: v1.63.1-eksbuild.1
  - name: coredns_version
    version: v1.11.4-eksbuild.14
  - name: kube_proxy_version
    version: v1.34.6-eksbuild.13
  - name: ami_release_version
    version: 1.34.9-20260801
  - name: kubernetes_version
    version: "1.34"
`

func repo(t *testing.T, files map[string]string) captain.Repo {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return captain.Repo{Dir: dir}
}

func copyFile(t *testing.T, from string) string {
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func render(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		mark := "x"
		if r.OK() {
			mark = "ok"
		}
		b.WriteString(r.Name + " " + r.Current + " " + r.Planned + " " + mark + "\n")
	}
	return b.String()
}

var live = fakeCluster{
	server:   "1.33",
	releases: map[string]string{"argocd": "9.3.7", "glueops-platform": "0.84.0", "calico": "v3.31.4"},
	labels:   map[string]string{"glueops-core/argocd-server/app.kubernetes.io/version": "v3.2.12"},
	images:   map[string]string{"tigera-operator/tigera-operator": "quay.io/tigera/operator:v1.40.7"},
}

func TestCollectEKSCaptain(t *testing.T) {
	r := repo(t, map[string]string{
		"VERSIONS/glueops.yaml":                  glueopsYAML,
		"VERSIONS/aws.yaml":                      awsYAML,
		"terraform/kubernetes/main.tf":           copyFile(t, "../aws/testdata/main.tf"),
		"terraform/kubernetes/terraform.tfstate": copyFile(t, "../aws/testdata/terraform.tfstate"),
	})
	var notes []string
	rows, err := Collect(context.Background(), r, live, func(s string) { notes = append(notes, s) })
	if err != nil {
		t.Fatal(err)
	}
	want := `csi_driver_version v1.45.0-eksbuild.2 v1.63.1-eksbuild.1 x
coredns_version v1.11.4-eksbuild.14 v1.11.4-eksbuild.14 ok
kube_proxy_version N/A v1.34.6-eksbuild.13 x
ami_release_version 1.33.0-20250519 1.34.9-20260801 x
eks_version 1.33 1.34 x
argocd_version 9.3.7 9.3.7 ok
argocd_app_version v3.2.12 v3.2.12 ok
glueops_platform_version v0.84.0 v0.85.0 x
calico_helm_chart_version v3.31.4 v3.31.4 ok
tigera_operator_version v1.40.7 v1.40.7 ok
calico_ctl_version N/A* v3.31.4 x
terraform_module_version v0.50.0 v0.51.0 x
`
	if got := render(rows); got != want {
		t.Fatalf("rows:\n%s\nwant:\n%s", got, want)
	}
	if len(notes) != 0 {
		t.Fatalf("notes: %v", notes)
	}
}

func TestCollectK3dCaptain(t *testing.T) {
	// venus-like: no main.tf, no tfstate, cluster unreachable
	r := repo(t, map[string]string{"VERSIONS/glueops.yaml": glueopsYAML, "VERSIONS/aws.yaml": awsYAML})
	var notes []string
	rows, err := Collect(context.Background(), r, fakeCluster{fail: true}, func(s string) { notes = append(notes, s) })
	if err != nil {
		t.Fatal(err)
	}
	want := `eks_version N/A 1.34 x
argocd_version N/A 9.3.7 x
argocd_app_version N/A v3.2.12 x
glueops_platform_version N/A v0.85.0 x
calico_helm_chart_version N/A v3.31.4 x
tigera_operator_version N/A v1.40.7 x
calico_ctl_version N/A* v3.31.4 x
`
	if got := render(rows); got != want {
		t.Fatalf("rows:\n%s", got)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "main.tf") {
		t.Fatalf("notes: %v", notes)
	}
}

func TestCollectNeedsGlueOpsVersions(t *testing.T) {
	if _, err := Collect(context.Background(), repo(t, nil), live, func(string) {}); err == nil {
		t.Fatal("want an error outside a captain repo")
	}
}

func TestCollectMissingPinIsNoted(t *testing.T) {
	r := repo(t, map[string]string{"VERSIONS/glueops.yaml": "versions:\n  - name: argocd_helm_chart_version\n    version: 9.3.7\n"})
	var notes []string
	rows, err := Collect(context.Background(), r, live, func(s string) { notes = append(notes, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "argocd_version" {
		t.Fatalf("rows %v", rows)
	}
	if len(notes) < 3 {
		t.Fatalf("notes %v", notes)
	}
}
