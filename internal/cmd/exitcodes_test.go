package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/glueops/captain-utils/internal/crds"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/ui"
	"github.com/glueops/captain-utils/internal/upgrade"
)

// stubHelm lets the upgrade flows reach their confirm: a diff that changed, an upgrade that must not run.
type stubHelm struct{ upgraded *bool }

func (stubHelm) RepoUpdate(context.Context, ...string) error { return nil }
func (stubHelm) RepoAdd(string, string) error                { return nil }
func (stubHelm) Diff(_ context.Context, _ helmx.Release, _ helmx.DiffOptions, w io.Writer) (bool, error) {
	_, _ = io.WriteString(w, "changed\n")
	return true, nil
}
func (stubHelm) ReleaseExists(context.Context, helmx.Release) (bool, error) { return true, nil }
func (s stubHelm) Upgrade(_ context.Context, r helmx.Release) (*helmx.Result, error) {
	*s.upgraded = true
	return &helmx.Result{Name: r.Name, Namespace: r.Namespace, Status: "deployed", Revision: 2}, nil
}

// captain with every pin and file the commands read
func fullCaptain(t *testing.T) string {
	t.Helper()
	dir := captainDir(t)
	mustWrite(t, dir+"/VERSIONS/glueops.yaml", "versions:\n  - name: platform_crds_version\n    version: v0.1.6\n  - name: argocd_helm_chart_version\n    version: 9.3.7\n  - name: glueops_platform_helm_chart_version\n    version: v0.85.0\n  - name: calico_helm_chart_version\n    version: v3.31.4\n")
	mustWrite(t, dir+"/platform.yaml", "a: 1\n")
	return dir
}

func runCLI(t *testing.T, dir string, answers []string, tty bool, args ...string) (code int, out, errs string, upgraded bool) {
	t.Helper()
	var up bool
	code, out, errs = runWith(t, func(a *App) {
		a.NewHelm = func(string) upgrade.Helm { return stubHelm{upgraded: &up} }
		if tty {
			a.Prompt = &ui.Script{Answers: answers}
		} else {
			a.Prompt = &noTerminal{}
		}
	}, append([]string{"--captain-dir", dir}, args...)...)
	return code, out, errs, up
}

// Usage errors are exit 2 for every command, never 1: crds check's 1 means drift, so a gate
// would read a bad command line as drift and go on to apply.
func TestUsageErrorsExit2(t *testing.T) {
	dir := fullCaptain(t)
	for _, args := range [][]string{
		{"crds", "check", "--bogus"},
		{"crds", "chek"},
		{"crds", "check", "--wait-timeout", "abc"},
		{"crds", "check", "--env", "prod"},
		{"crds", "check", "--version", "v0.1.6", "--chart-dir", "/x"},
		{"crds", "check", "extra"},
		{"platform", "diff", "--bogus"},
		{"platform", "chek"},
		{"platform", "diff", "--version", "v1", "--chart-dir", "/x"},
		{"bogus"},
		{"aws", "calico", "bogus"},
		{"crds", "apply", "-o", "yaml"},
		{"platform", "diff", "-o", "yaml"},
		{"pods", "list", "-o", "yaml"},
		{"versions", "diff", "-o", "yaml"},
	} {
		if code, out, errs, _ := runCLI(t, dir, nil, true, args...); code != 2 || out != "" || errs == "" {
			t.Errorf("%v: exit %d out %q err %q, want exit 2", args, code, out, errs)
		}
	}
}

func TestWithoutTerminalAVersionIsRequired(t *testing.T) {
	dir := fullCaptain(t)
	for _, args := range [][]string{
		{"crds", "check"}, {"crds", "apply", "--yes"}, {"platform", "upgrade", "--yes"}, {"platform", "diff"}, {"argocd", "upgrade", "--yes"}, {"argocd", "diff"},
	} {
		code, _, errs, up := runCLI(t, dir, nil, false, args...)
		if code != 2 || !strings.Contains(errs, "--version") || up {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
	// with --version they run (the fake bundle has a CRD the empty cluster lacks: check reports drift)
	if code, out, _, _ := runCLI(t, dir, nil, false, "crds", "check", "--version", "v0.1.6"); code != 1 || !strings.Contains(out, "1 new, 0 changed") {
		t.Fatalf("%d %q", code, out)
	}
	if code, out, errs, _ := runCLI(t, dir, nil, false, "platform", "diff", "--version", "v0.85.0"); code != 0 || !strings.Contains(out, "changed") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if code, _, errs, up := runCLI(t, dir, nil, false, "argocd", "upgrade", "--version", "9.3.7", "--yes"); code != 0 || !up {
		t.Fatalf("%d %q %v", code, errs, up)
	}
}

// A declined confirm exits 3, so "applied" and "skipped" differ; nothing is installed.
func TestDeclinedConfirmExits3(t *testing.T) {
	dir := fullCaptain(t)
	for _, args := range [][]string{
		{"crds", "apply", "--version", "v0.1.6"},
		{"platform", "upgrade", "--version", "v0.85.0"},
		{"argocd", "upgrade", "--version", "9.3.7"},
		{"aws", "calico", "upgrade"},
	} {
		code, _, errs, up := runCLI(t, dir, []string{"no"}, true, args...)
		if code != ExitDeclined || up {
			t.Errorf("%v: exit %d upgraded %v %q", args, code, up, errs)
		}
	}
}

func TestCRDsApplyEndsWithTheResult(t *testing.T) {
	dir := fullCaptain(t)
	code, out, _, _ := runCLI(t, dir, []string{"no"}, true, "crds", "apply", "--version", "v0.1.6")
	if code != ExitDeclined || !strings.HasSuffix(strings.TrimSpace(out), "applied=0 unchanged=0 failed=0") {
		t.Fatalf("%d %q", code, out)
	}
	// -o json: stdout is the result alone, the plan and diff go to stderr
	code, out, errs, _ := runCLI(t, dir, []string{"no"}, true, "crds", "apply", "--version", "v0.1.6", "-o", "json")
	var res crds.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != ExitDeclined {
		t.Fatalf("%d %v %q", code, err, out)
	}
	if res.Version != "v0.1.6" || res.Applied == nil || res.Unchanged == nil || res.Failed == nil || res.Established || !strings.Contains(errs, "1 new, 0 changed") {
		t.Fatalf("%+v\n%s", res, errs)
	}
}

// Only the explicit --yes approves: an exported variable outlives the command it was set for.
func TestApprovalEnvironmentVariablesDoNotApprove(t *testing.T) {
	dir := fullCaptain(t)
	t.Setenv("CAPTAIN_UTILS_YES", "true")
	t.Setenv("CRDS_AUTO_CONFIRM", "yes")
	for _, args := range [][]string{
		{"crds", "apply", "--version", "v0.1.6"},
		{"platform", "upgrade", "--version", "v0.85.0"},
		{"argocd", "upgrade", "--version", "9.3.7"},
		{"aws", "calico", "upgrade"},
	} {
		// at a terminal it still asks (the scripted "no" is the answer) ...
		code, _, errs, up := runCLI(t, dir, []string{"no"}, true, args...)
		if code != ExitDeclined || up || strings.Contains(errs, "(--yes)") {
			t.Errorf("%v at a terminal: exit %d upgraded %v %q", args, code, up, errs)
		}
		// ... and without one it refuses
		code, _, errs, up = runCLI(t, dir, nil, false, args...)
		if code != ExitUsage || up || !strings.Contains(errs, "pass --yes") {
			t.Errorf("%v without a terminal: exit %d upgraded %v %q", args, code, up, errs)
		}
	}
	// the flag still works
	if code, _, _, up := runCLI(t, dir, nil, false, "argocd", "upgrade", "--version", "9.3.7", "--yes"); code != 0 || !up {
		t.Fatalf("--yes: %d %v", code, up)
	}
	if code, _, _, up := runCLI(t, dir, nil, false, "argocd", "upgrade", "--version", "9.3.7", "-y"); code != 0 || !up {
		t.Fatalf("-y: %d %v", code, up)
	}
}

func TestInterruptAndFailureStayOutOfTheUsageCode(t *testing.T) {
	// a command that started and failed is 1, not 2
	var out, errb bytes.Buffer
	a := NewApp("v0.0.0-test", "abc", &out, &errb)
	a.NewCluster = func(string) (Cluster, error) { return fakeCluster{}, nil }
	a.Prompt = &ui.Script{}
	if code := a.Execute(context.Background(), []string{"--captain-dir", t.TempDir(), "versions", "diff"}); code != 1 {
		t.Fatalf("%d %q", code, errb.String())
	}
}
