package cmd

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/glueops/captain-utils/internal/kube"
	"github.com/glueops/captain-utils/internal/ui"
)

type fakeCluster struct{ server string }

func (f fakeCluster) ServerVersion(context.Context) (string, error) {
	if f.server == "" {
		return "", errors.New("unreachable")
	}
	return f.server, nil
}
func (fakeCluster) ReleaseChartVersion(_ context.Context, r string) (string, error) {
	return map[string]string{"argocd": "9.3.7"}[r], nil
}
func (fakeCluster) DeploymentLabel(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (fakeCluster) DeploymentImage(context.Context, string, string) (string, error) { return "", nil }
func (fakeCluster) DeleteDaemonSet(context.Context, string, string) (bool, error)   { return true, nil }
func (fakeCluster) Apply(_ context.Context, o *unstructured.Unstructured, _ kube.ApplyOptions) (*unstructured.Unstructured, error) {
	return o, nil
}
func (fakeCluster) ListCRDs(context.Context) ([]*unstructured.Unstructured, error) { return nil, nil }
func (fakeCluster) RemoveCRDAnnotation(context.Context, string, string) error      { return nil }
func (fakeCluster) MigrateCRDToSSA(context.Context, string, string) (bool, error)  { return false, nil }
func (fakeCluster) RemoveCRDLabel(context.Context, string, string) error           { return nil }
func (fakeCluster) WaitCRDsDeleted(context.Context, []string, time.Duration) error { return nil }
func (fakeCluster) WaitCRDsEstablished(context.Context, []string, time.Duration) error {
	return nil
}
func (fakeCluster) Pods(context.Context, time.Time) ([]kube.PodRow, error) {
	return []kube.PodRow{
		{Namespace: "glueops-core", Name: "ok", Ready: "1/1", Status: "Running", Age: "1d"},
		{Namespace: "nonprod", Name: "broken", Ready: "0/1", Status: "CrashLoopBackOff", Restarts: 7, Age: "2h"},
	}, nil
}

func run(t *testing.T, cl Cluster, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	var out, errb bytes.Buffer
	a := NewApp("v0.0.0-test", "abc", &out, &errb)
	a.NewCluster = func(string) (Cluster, error) { return cl, nil }
	a.Prompt = &ui.Script{}
	code := a.Execute(context.Background(), args)
	return code, out.String(), errb.String()
}

func captainDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "VERSIONS/glueops.yaml"), "versions:\n  - name: argocd_helm_chart_version\n    version: 9.3.7\n  - name: terraform_module_version\n    version: v0.51.0\n")
	mustWrite(t, filepath.Join(dir, "VERSIONS/aws.yaml"), "versions:\n  - name: kubernetes_version\n    version: \"1.34\"\n  - name: ami_release_version\n    version: 1.34.9-20260801\n  - name: coredns_version\n    version: v9\n")
	src, err := os.ReadFile("../aws/testdata/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "terraform/kubernetes/main.tf"), string(src))
	return dir
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, nil, nil, "version")
	if code != 0 || out != "captain-utils v0.0.0-test (abc)\n" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestBadEnv(t *testing.T) {
	code, _, errs := run(t, nil, map[string]string{"environment": "staging"}, "version")
	if code != 1 || !strings.Contains(errs, "--env must be") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestEnvFlagBeatsEnvironmentVariable(t *testing.T) {
	code, _, errs := run(t, nil, map[string]string{"environment": "staging"}, "--env", "dev", "version")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestVersionsDiffCSV(t *testing.T) {
	dir := captainDir(t)
	code, out, errs := run(t, fakeCluster{server: "1.33"}, nil, "versions", "diff", "-o", "csv", "--captain-dir", dir)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, r := range recs {
		got = append(got, strings.Join(r, "|"))
	}
	want := "Tool Name|Current|Planned|Status ami_release_version|1.33.0-20250519|1.34.9-20260801|❌ eks_version|1.33|1.34|❌ argocd_version|9.3.7|9.3.7|✅ terraform_module_version|v0.50.0|v0.51.0|❌"
	if strings.Join(got, " ") != want {
		t.Fatalf("csv:\n%s", out)
	}
	// main.tf untouched (the Python helper rewrote it on every run)
	b, _ := os.ReadFile(filepath.Join(dir, "terraform/kubernetes/main.tf"))
	src, _ := os.ReadFile("../aws/testdata/main.tf")
	if !bytes.Equal(b, src) {
		t.Fatal("versions diff changed main.tf")
	}
}

func TestVersionsDiffTableOutsideCaptainRepo(t *testing.T) {
	code, _, errs := run(t, fakeCluster{}, nil, "versions", "diff", "--captain-dir", t.TempDir())
	if code != 1 || !strings.Contains(errs, "captain repo root") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestAWSKubernetesDryRunThenWrite(t *testing.T) {
	dir := captainDir(t)
	tf := filepath.Join(dir, "terraform/kubernetes/main.tf")
	before, _ := os.ReadFile(tf)

	code, out, errs := run(t, nil, nil, "aws", "kubernetes", "--dry-run", "--captain-dir", dir)
	if code != 0 || !strings.Contains(out, `-  eks_version        = "1.33"`) || !strings.Contains(out, `+  eks_version        = "1.34"`) {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	if b, _ := os.ReadFile(tf); !bytes.Equal(b, before) {
		t.Fatal("--dry-run wrote main.tf")
	}

	if code, _, errs = run(t, nil, nil, "aws", "upgrade-kubernetes", "--captain-dir", dir); code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	b, _ := os.ReadFile(tf)
	if !strings.Contains(string(b), `eks_version        = "1.34"`) {
		t.Fatal("not written")
	}
	if fi, _ := os.Stat(tf); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// second run: nothing to do
	if code, out, errs = run(t, nil, nil, "aws", "kubernetes", "--captain-dir", dir); code != 0 || out != "" || !strings.Contains(errs, "already matches") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}

func TestAWSAddonsWarnsMissingPins(t *testing.T) {
	dir := captainDir(t)
	code, out, errs := run(t, nil, nil, "aws", "eks-addons", "--dry-run", "--captain-dir", dir)
	if code != 0 || !strings.Contains(out, `+  coredns_version    = "v9"`) || !strings.Contains(errs, "csi_driver_version not found") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
}

func TestAWSNodepoolsNeedsClusterOrFlag(t *testing.T) {
	dir := captainDir(t)
	code, _, errs := run(t, fakeCluster{}, nil, "aws", "nodepools", "--dry-run", "--captain-dir", dir)
	if code != 1 || !strings.Contains(errs, "--kubernetes-version") {
		t.Fatalf("%d %q", code, errs)
	}
	code, out, errs := run(t, fakeCluster{}, nil, "aws", "upgrade-eks-nodepools", "--dry-run", "--kubernetes-version", "1.33", "--captain-dir", dir)
	if code != 0 || !strings.Contains(out, `+      "name" : "glueops-platform-node-pool-3",`) {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
}

func TestPodsWatchOnce(t *testing.T) {
	code, out, errs := run(t, fakeCluster{}, nil, "pods", "watch", "--once")
	if code != 0 || !strings.Contains(out, "broken") || strings.Contains(out, " ok ") || !strings.HasPrefix(out, "NAMESPACE") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	code, out, _ = run(t, fakeCluster{}, nil, "pods", "watch", "--once", "--all")
	if code != 0 || !strings.Contains(out, "glueops-core") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestPlatformFlagsExclusive(t *testing.T) {
	code, _, errs := run(t, fakeCluster{}, nil, "platform", "upgrade", "--version", "v1", "--chart-dir", "/x")
	if code != 1 || !strings.Contains(errs, "none of the others can be") {
		t.Fatalf("%d %q", code, errs)
	}
}
