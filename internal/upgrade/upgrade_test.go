package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/ui"
)

// fakeHelm records calls the way the bash tests' helm stub printed "HELM ..." lines.
type fakeHelm struct {
	calls     []string
	changed   bool
	diffErr   error
	upErr     error
	updateErr error
}

func (f *fakeHelm) RepoUpdate(_ context.Context, names ...string) error {
	f.calls = append(f.calls, "repo update "+strings.Join(names, " "))
	return f.updateErr
}
func (f *fakeHelm) RepoAdd(name, url string) error {
	f.calls = append(f.calls, "repo add "+name+" "+url)
	return nil
}
func (f *fakeHelm) Diff(_ context.Context, r helmx.Release, _ helmx.DiffOptions, w io.Writer) (bool, error) {
	f.calls = append(f.calls, "diff "+strings.TrimPrefix(r.Command(), "helm upgrade --install "))
	fmt.Fprintln(w, "glueops-core, x, ConfigMap (v1) has changed:")
	return f.changed, f.diffErr
}
func (f *fakeHelm) Upgrade(_ context.Context, r helmx.Release) (*helmx.Result, error) {
	f.calls = append(f.calls, "upgrade "+strings.TrimPrefix(r.Command(), "helm upgrade --install "))
	if f.upErr != nil {
		return nil, f.upErr
	}
	return &helmx.Result{Name: r.Name, Namespace: r.Namespace, Status: "deployed", Revision: 2}, nil
}

type fakeKube struct {
	calls   []string
	missing bool
}

func (f *fakeKube) DeleteDaemonSet(_ context.Context, ns, name string) (bool, error) {
	f.calls = append(f.calls, "delete ds "+ns+"/"+name)
	return !f.missing, nil
}

type harness struct {
	d      *Deps
	helm   *fakeHelm
	kube   *fakeKube
	prompt *ui.Script
	out    *bytes.Buffer
	errs   *bytes.Buffer
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
`

func newHarness(t *testing.T, files map[string]string, answers ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	if files == nil {
		files = map[string]string{"VERSIONS/glueops.yaml": glueopsYAML, "platform.yaml": "a: 1\n"}
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{helm: &fakeHelm{changed: true}, kube: &fakeKube{}, prompt: &ui.Script{Answers: answers}, out: &bytes.Buffer{}, errs: &bytes.Buffer{}}
	h.d = &Deps{
		Helm: h.helm, Kube: h.kube,
		Prompt: h.prompt, P: ui.New(h.out, h.errs, true), Repo: captain.Repo{Dir: dir},
		GitInfo: func(string) string { return "main@abc1234, dirty" },
	}
	return h
}

func (h *harness) path(p string) string { return filepath.Join(h.d.Repo.Dir, p) }

func (h *harness) want(t *testing.T, got []string, want ...string) {
	t.Helper()
	g := strings.Join(got, "\n")
	w := strings.ReplaceAll(strings.Join(want, "\n"), "$DIR", h.d.Repo.Dir)
	if g != w {
		t.Fatalf("calls:\n%s\nwant:\n%s\nstderr:\n%s", g, w, h.errs.String())
	}
}

// P0: the pinned production path: one pinned version, platform.yaml twice without overrides.yaml
func TestPlatformPinnedNoOverrides(t *testing.T) {
	h := newHarness(t, nil, "v0.85.0", "yes")
	if err := Platform(context.Background(), h.d, PlatformOptions{}, h.out); err != nil {
		t.Fatal(err)
	}
	h.want(t, h.helm.calls,
		"repo update glueops-platform",
		"diff glueops-platform glueops-platform/glueops-platform --version v0.85.0 -f $DIR/platform.yaml -f $DIR/platform.yaml -n glueops-core --create-namespace",
		"upgrade glueops-platform glueops-platform/glueops-platform --version v0.85.0 -f $DIR/platform.yaml -f $DIR/platform.yaml -n glueops-core --create-namespace",
	)
	if got := h.prompt.Asked[0]; got != "CHOOSE glueops-platform version [v0.85.0|custom|Back]" {
		t.Fatalf("asked %q", got)
	}
	if !strings.Contains(h.errs.String(), "No Overrides.yaml detected") || len(h.prompt.Paged) != 1 {
		t.Fatalf("stderr %q paged %d", h.errs.String(), len(h.prompt.Paged))
	}
	if !strings.Contains(h.out.String(), "chosen version: v0.85.0 for glueops-platform/glueops-platform") {
		t.Fatalf("out %q", h.out.String())
	}
}

func TestPlatformOverridesAndDecline(t *testing.T) {
	h := newHarness(t, map[string]string{"VERSIONS/glueops.yaml": glueopsYAML, "platform.yaml": "", "overrides.yaml": ""}, "v0.85.0", "no")
	if err := Platform(context.Background(), h.d, PlatformOptions{}, h.out); !errors.Is(err, ui.ErrDeclined) {
		t.Fatalf("a declined confirm must be ui.ErrDeclined (exit 3): %v", err)
	}
	h.want(t, h.helm.calls,
		"repo update glueops-platform",
		"diff glueops-platform glueops-platform/glueops-platform --version v0.85.0 -f $DIR/platform.yaml -f $DIR/overrides.yaml -n glueops-core --create-namespace",
	)
	if !strings.Contains(h.errs.String(), "Overrides.yaml detected") {
		t.Fatal(h.errs.String())
	}
}

func TestPlatformBack(t *testing.T) {
	h := newHarness(t, nil, "Back")
	if err := Platform(context.Background(), h.d, PlatformOptions{}, h.out); err != nil || len(h.helm.calls) != 0 {
		t.Fatalf("%v %v", err, h.helm.calls)
	}
}

func TestPlatformNoTerminalNeedsFlags(t *testing.T) {
	h := newHarness(t, nil)
	h.d.Prompt = noTTY{&ui.Script{}}
	err := Platform(context.Background(), h.d, PlatformOptions{}, h.out)
	if !errors.Is(err, ui.ErrNoTerminal) || !errors.Is(err, ui.ErrUsage) || !strings.Contains(err.Error(), "--version") || len(h.helm.calls) != 0 {
		t.Fatalf("no --version without a terminal is a usage error that changes nothing: %v %v", err, h.helm.calls)
	}
	// with --version and no --yes: refuses at the confirm, after the diff
	err = Platform(context.Background(), h.d, PlatformOptions{Version: "v0.85.0"}, h.out)
	if !errors.Is(err, ui.ErrNoTerminal) || errors.Is(err, ui.ErrUsage) || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("%v", err)
	}
	h.d.Yes = true
	if err := Platform(context.Background(), h.d, PlatformOptions{Version: "v0.85.0"}, h.out); err != nil {
		t.Fatal(err)
	}
	if last := h.helm.calls[len(h.helm.calls)-1]; !strings.HasPrefix(last, "upgrade ") {
		t.Fatal(h.helm.calls)
	}
}

func TestPlatformDiffOnlyPrintsAndStops(t *testing.T) {
	h := newHarness(t, nil)
	h.d.Prompt = noTTY{&ui.Script{}}
	if err := Platform(context.Background(), h.d, PlatformOptions{Version: "v0.85.0", DiffOnly: true}, h.out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "has changed") || strings.HasPrefix(h.helm.calls[len(h.helm.calls)-1], "upgrade") {
		t.Fatalf("%q %v", h.out.String(), h.helm.calls)
	}
}

func chartDir(t *testing.T, chart string) string {
	dir := filepath.Join(t.TempDir(), "platform chart")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if chart != "" {
		if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chart), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// D-series: the custom local-directory flow
func TestPlatformCustomFromMenu(t *testing.T) {
	dir := chartDir(t, "name: glueops-platform\nversion: 0.86.0\n")
	h := newHarness(t, nil, "custom", dir, "yes", "yes")
	if err := Platform(context.Background(), h.d, PlatformOptions{}, h.out); err != nil {
		t.Fatal(err)
	}
	h.want(t, h.helm.calls,
		"diff glueops-platform "+dir+" -f $DIR/platform.yaml -f $DIR/platform.yaml -n glueops-core --create-namespace --description \"custom: "+dir+" (main@abc1234, dirty)\"",
		"upgrade glueops-platform "+dir+" -f $DIR/platform.yaml -f $DIR/platform.yaml -n glueops-core --create-namespace --description \"custom: "+dir+" (main@abc1234, dirty)\"",
	)
	for _, s := range []string{"custom installs an UNRELEASED chart", "chart glueops-platform 0.86.0 from " + dir, "helm list will show glueops-platform-0.86.0", "installed from " + dir} {
		if !strings.Contains(h.errs.String(), s) {
			t.Errorf("stderr lacks %q:\n%s", s, h.errs.String())
		}
	}
	if h.prompt.Asked[2] != "CONFIRM Diff this chart against the cluster?" || h.prompt.Asked[3] != "CONFIRM Apply upgrade" {
		t.Fatal(h.prompt.Asked)
	}
}

func TestPlatformCustomRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ dir, want string }{
		"not a dir":  {filepath.Join(t.TempDir(), "nope"), "is not a directory"},
		"no Chart":   {chartDir(t, ""), "has no Chart.yaml"},
		"no name":    {chartDir(t, "version: 1.0.0\n"), "cannot read .name"},
		"no version": {chartDir(t, "name: x\n"), "cannot read .version"},
		"bad yaml":   {chartDir(t, "name: [\n"), "cannot read .name"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			err := Platform(context.Background(), h.d, PlatformOptions{ChartDir: tc.dir}, h.out)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(h.helm.calls) != 0 {
				t.Fatalf("%v %v", err, h.helm.calls)
			}
		})
	}
}

func TestPlatformCustomNameMismatchAndDecline(t *testing.T) {
	dir := chartDir(t, "name: other\nversion: 1.0.0\n")
	h := newHarness(t, nil, "no")
	if err := Platform(context.Background(), h.d, PlatformOptions{ChartDir: dir}, h.out); !errors.Is(err, ui.ErrDeclined) {
		t.Fatal(err)
	}
	if len(h.helm.calls) != 0 || !strings.Contains(h.errs.String(), "Chart.yaml name is 'other'") || !strings.Contains(h.errs.String(), "UNRELEASED") {
		t.Fatalf("%v\n%s", h.helm.calls, h.errs.String())
	}
}

func TestPlatformCustomEmptyPathIsBack(t *testing.T) {
	h := newHarness(t, nil, "custom", "")
	if err := Platform(context.Background(), h.d, PlatformOptions{}, h.out); err != nil || len(h.helm.calls) != 0 {
		t.Fatalf("%v %v", err, h.helm.calls)
	}
}

func TestPlatformUpgradeFailure(t *testing.T) {
	h := newHarness(t, nil, "v0.85.0", "yes")
	h.helm.upErr = errors.New("UPGRADE FAILED: boom")
	if err := Platform(context.Background(), h.d, PlatformOptions{}, h.out); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatal(err)
	}
}

// ArgoCD's CRDs come from the platform-crds bundle: this flow never applies any
func TestArgocdPinnedAppliesNoCRDs(t *testing.T) {
	h := newHarness(t, nil, "9.3.7", "yes")
	if err := Argocd(context.Background(), h.d, ArgocdOptions{}, h.out); err != nil {
		t.Fatal(err)
	}
	h.want(t, h.helm.calls,
		"repo update argo",
		"diff argocd argo/argo-cd --version 9.3.7 -f $DIR/argocd.yaml -n glueops-core --create-namespace --skip-crds",
		"upgrade argocd argo/argo-cd --version 9.3.7 -f $DIR/argocd.yaml -n glueops-core --create-namespace --skip-crds",
	)
	if len(h.kube.calls) != 0 || !strings.Contains(h.errs.String(), "managed by the platform-crds bundle") {
		t.Fatalf("%v %s", h.kube.calls, h.errs.String())
	}
	if h.prompt.Asked[0] != "CHOOSE argo-cd chart version [9.3.7|Back]" || len(h.prompt.Asked) != 2 {
		t.Fatal(h.prompt.Asked)
	}
}

func TestArgocdBackAndDecline(t *testing.T) {
	h := newHarness(t, nil, "Back")
	if err := Argocd(context.Background(), h.d, ArgocdOptions{}, h.out); err != nil || len(h.helm.calls) != 0 {
		t.Fatalf("%v %v", err, h.helm.calls)
	}
	h = newHarness(t, nil, "9.3.7", "no")
	if err := Argocd(context.Background(), h.d, ArgocdOptions{}, h.out); !errors.Is(err, ui.ErrDeclined) || strings.HasPrefix(h.helm.calls[len(h.helm.calls)-1], "upgrade") {
		t.Fatalf("%v %v", err, h.helm.calls)
	}
}

func TestArgocdNoTerminalNeedsVersion(t *testing.T) {
	h := newHarness(t, nil)
	h.d.Prompt = noTTY{&ui.Script{}}
	err := Argocd(context.Background(), h.d, ArgocdOptions{}, h.out)
	if !errors.Is(err, ui.ErrUsage) || !strings.Contains(err.Error(), "--version") || len(h.helm.calls) != 0 {
		t.Fatalf("%v %v", err, h.helm.calls)
	}
	h.d.Yes = true
	if err := Argocd(context.Background(), h.d, ArgocdOptions{Version: "9.3.7"}, h.out); err != nil {
		t.Fatal(err)
	}
	if last := h.helm.calls[len(h.helm.calls)-1]; !strings.HasPrefix(last, "upgrade argocd") {
		t.Fatal(h.helm.calls)
	}
}

const calicoDiff = "diff calico projectcalico/tigera-operator --version v3.31.4 -f $DIR/calico.yaml -n tigera-operator --create-namespace"

func TestCalicoShowsTheDiffBeforeItConfirms(t *testing.T) {
	h := newHarness(t, nil, "yes")
	if err := Calico(context.Background(), h.d, h.out); err != nil {
		t.Fatal(err)
	}
	h.want(t, h.kube.calls, "delete ds kube-system/aws-node")
	h.want(t, h.helm.calls,
		"repo add projectcalico https://docs.tigera.io/calico/charts",
		"repo update projectcalico",
		calicoDiff,
		"upgrade calico projectcalico/tigera-operator --version v3.31.4 -f $DIR/calico.yaml -n tigera-operator --create-namespace",
	)
	if len(h.prompt.Paged) != 1 || len(h.prompt.Asked) != 1 || h.prompt.Asked[0] != "CONFIRM Remove the aws-node daemonset (kube-system) and apply this calico v3.31.4 upgrade?" {
		t.Fatalf("paged %d asked %v", len(h.prompt.Paged), h.prompt.Asked)
	}
}

func TestCalicoDeclineDeletesAndInstallsNothing(t *testing.T) {
	h := newHarness(t, nil, "no")
	err := Calico(context.Background(), h.d, h.out)
	if !errors.Is(err, ui.ErrDeclined) || len(h.kube.calls) != 0 || strings.HasPrefix(h.helm.calls[len(h.helm.calls)-1], "upgrade") {
		t.Fatalf("declined: %v %v %v", err, h.kube.calls, h.helm.calls)
	}
	if len(h.prompt.Paged) != 1 {
		t.Fatal("the diff must be shown before the question")
	}
}

func TestCalicoDiffOrRepoFailureDeletesAndInstallsNothing(t *testing.T) {
	for name, set := range map[string]func(*fakeHelm){
		"diff failure":    func(f *fakeHelm) { f.diffErr = errors.New("diff boom") },
		"refresh failure": func(f *fakeHelm) { f.updateErr = errors.New("refresh boom") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, "yes")
			set(h.helm)
			err := Calico(context.Background(), h.d, h.out)
			if err == nil || !strings.Contains(err.Error(), "boom") || len(h.kube.calls) != 0 || len(h.prompt.Asked) != 0 {
				t.Fatalf("%v %v %v", err, h.kube.calls, h.prompt.Asked)
			}
			for _, c := range h.helm.calls {
				if strings.HasPrefix(c, "upgrade") {
					t.Fatal("installed after a failure:", h.helm.calls)
				}
			}
		})
	}
}

func TestCalicoNoTerminalWithYesPrintsTheDiffThenApplies(t *testing.T) {
	h := newHarness(t, nil)
	h.d.Prompt = noTTY{&ui.Script{}}
	h.d.Yes = true
	if err := Calico(context.Background(), h.d, h.out); err != nil {
		t.Fatal(err)
	}
	calls := h.helm.calls
	if calls[len(calls)-2] != strings.ReplaceAll(calicoDiff, "$DIR", h.d.Repo.Dir) || !strings.HasPrefix(calls[len(calls)-1], "upgrade calico") || len(h.kube.calls) != 1 {
		t.Fatalf("%v %v", calls, h.kube.calls)
	}
}

type noTTY struct{ *ui.Script }

func (noTTY) Interactive() bool { return false }
