package cmd

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glueops/captain-utils/internal/ui"
)

// menuPrompt is a Script that can also answer Esc (ui.Back) where the menu offers no Back option.
type menuPrompt struct{ ui.Script }

func (m *menuPrompt) Choose(title string, options ...string) (string, error) {
	if len(m.Answers) > 0 && m.Answers[0] == ui.Back {
		m.Asked = append(m.Asked, "CHOOSE "+title+" ["+strings.Join(options, "|")+"]")
		m.Answers = m.Answers[1:]
		return ui.Back, nil
	}
	return m.Script.Choose(title, options...)
}

type noTTY struct{ menuPrompt }

func (noTTY) Interactive() bool { return false }

// menuRig is a menu whose subcommands and preflight binary are recorded, not run.
type menuRig struct {
	m      *menu
	prompt *menuPrompt
	errs   *bytes.Buffer
	out    *bytes.Buffer
	runs   []string // the subcommand args, joined
	pre    [][]string
	rc     map[string]int // exit code by joined args (default 0)
	preRC  int
}

func newMenuRig(t *testing.T, dir string, answers ...string) *menuRig {
	t.Helper()
	r := &menuRig{prompt: &menuPrompt{Script: ui.Script{Answers: answers}}, errs: &bytes.Buffer{}, out: &bytes.Buffer{}, rc: map[string]int{}}
	a := NewApp("v0.0.0-test", "abc", r.out, r.errs)
	a.Prompt = r.prompt
	a.Root()
	a.v.Set("captain-dir", dir)
	a.p = ui.New(r.out, r.errs, true)
	r.m = a.newMenu()
	r.m.run = func(_ context.Context, args ...string) int {
		k := strings.Join(args, " ")
		r.runs = append(r.runs, k)
		return r.rc[k]
	}
	r.m.preflight = func(_ context.Context, bin string, args []string) int {
		r.pre = append(r.pre, append([]string{bin}, args...))
		return r.preRC
	}
	r.m.lookPath = func(s string) (string, error) { return "/usr/bin/" + s, nil }
	r.m.getenv = func(string) string { return "" }
	return r
}

func (r *menuRig) run() int { return r.m.Run(context.Background()) }

func glueopsPins(t *testing.T, pins string) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "VERSIONS/glueops.yaml"), "versions:\n"+pins)
	return dir
}

func TestMenuNeedsATerminal(t *testing.T) {
	r := newMenuRig(t, t.TempDir())
	r.prompt = nil
	r.m.a.Prompt = &noTTY{}
	if code := r.run(); code != 1 || !strings.Contains(r.errs.String(), "needs a terminal") {
		t.Fatalf("%d %q", code, r.errs.String())
	}
}

func TestMenuProductionItemsInOrderAndTheSubcommandsTheyRun(t *testing.T) {
	r := newMenuRig(t, t.TempDir(),
		"show_diff_table", "argocd", "glueops-platform", "inspect_pods", "Exit")
	if code := r.run(); code != 0 {
		t.Fatalf("%d %s", code, r.errs.String())
	}
	want := []string{"versions diff", "argocd upgrade", "platform upgrade", "pods watch"}
	if strings.Join(r.runs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("runs:\n%s", strings.Join(r.runs, "\n"))
	}
	asked := r.prompt.Asked
	// no dev/production chooser: the menu opens directly on the item list
	if asked[0] != "CHOOSE  [show_diff_table|crds|argocd|glueops-platform|app_chart_preflight|aws|inspect_pods|Exit]" {
		t.Fatalf("menu: %q", asked[0])
	}
	if !strings.Contains(r.errs.String(), "Showing diff table before proceeding") || !strings.Contains(r.out.String(), "Goodbye!") {
		t.Fatalf("out %q err %q", r.out.String(), r.errs.String())
	}
}

func TestMenuEscAtAMenuLeavesItLikeGumDid(t *testing.T) {
	for _, tc := range [][]string{{ui.Back}, {"aws", ui.Back}} {
		r := newMenuRig(t, t.TempDir(), tc...)
		if code := r.run(); code != 0 || len(r.runs) != 0 || !strings.Contains(r.out.String(), "Goodbye!") {
			t.Fatalf("%v: %d runs %v out %q err %q", tc, code, r.runs, r.out.String(), r.errs.String())
		}
	}
}

func TestMenuAStepThatFailsReturnsToTheMenu(t *testing.T) {
	r := newMenuRig(t, t.TempDir(), "argocd", "glueops-platform", "Exit")
	r.rc["argocd upgrade"] = 1
	if code := r.run(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Join(r.runs, "|") != "argocd upgrade|platform upgrade" {
		t.Fatalf("a failed step ended the menu: %v", r.runs)
	}
}

func TestMenuAWSOptionsRunTheirSubcommandsAndExitEndsTheMenu(t *testing.T) {
	r := newMenuRig(t, t.TempDir(),
		"aws", "calico", "aws", "eks-addons", "aws", "upgrade-eks-nodepools", "aws", "upgrade-kubernetes", "aws", "Exit")
	if code := r.run(); code != 0 {
		t.Fatalf("%d %s", code, r.errs.String())
	}
	want := "aws calico upgrade|aws eks-addons|aws nodepools|aws kubernetes"
	if strings.Join(r.runs, "|") != want {
		t.Fatalf("%v", r.runs)
	}
	if len(r.prompt.Answers) != 0 || !strings.Contains(r.out.String(), "Goodbye!") {
		t.Fatalf("aws → Exit did not end the menu (left %v)", r.prompt.Answers)
	}
	if got := r.prompt.Asked[1]; got != "CHOOSE  [calico|eks-addons|upgrade-eks-nodepools|upgrade-kubernetes|Exit]" {
		t.Fatalf("aws menu: %q", got)
	}
}

func TestMenuCRDsWithoutAValidPinSaysSoAndRunsNothing(t *testing.T) {
	for name, pins := range map[string]string{
		"no pin":       "  - name: argocd_helm_chart_version\n    version: 9.3.7\n",
		"not a tag":    "  - name: platform_crds_version\n    version: feat/otel-20260902\n",
		"missing file": "",
		"two pins":     "  - name: platform_crds_version\n    version: v0.1.6\n  - name: platform_crds_version\n    version: v0.1.7\n",
	} {
		dir := glueopsPins(t, pins)
		if name == "missing file" {
			dir = t.TempDir()
		}
		r := newMenuRig(t, dir, "crds", "Exit")
		if code := r.run(); code != 0 || len(r.runs) != 0 {
			t.Fatalf("%s: %d %v", name, code, r.runs)
		}
		if !strings.Contains(r.errs.String(), "could not determine the platform-crds version") {
			t.Fatalf("%s: %q", name, r.errs.String())
		}
	}
}

func TestMenuCRDsProductionAppliesThePinAndOffersCustomAndBack(t *testing.T) {
	dir := glueopsPins(t, "  - name: platform_crds_version\n    version: v0.1.6\n")
	r := newMenuRig(t, dir, "crds", "v0.1.6", "Exit")
	if code := r.run(); code != 0 || strings.Join(r.runs, "|") != "crds apply --version v0.1.6" {
		t.Fatalf("%d %v", code, r.runs)
	}
	if got := r.prompt.Asked[1]; got != "CHOOSE  [v0.1.6|custom|Back]" {
		t.Fatalf("crds menu: %q", got)
	}
	r = newMenuRig(t, dir, "crds", "Back", "Exit")
	if r.run(); len(r.runs) != 0 {
		t.Fatalf("Back still ran %v", r.runs)
	}
}

func TestMenuCRDsFailureNamesWhatWasNotAppliedAndGoesOn(t *testing.T) {
	dir := glueopsPins(t, "  - name: platform_crds_version\n    version: v0.1.6\n")
	r := newMenuRig(t, dir, "crds", "v0.1.6", "argocd", "Exit")
	r.rc["crds apply --version v0.1.6"] = 1
	if code := r.run(); code != 0 || len(r.runs) != 2 {
		t.Fatalf("%d %v", code, r.runs)
	}
	if !strings.Contains(r.errs.String(), "platform-crds v0.1.6 NOT applied, or only partly") {
		t.Fatalf("%q", r.errs.String())
	}
}

func TestMenuCRDsCustomWarnsAsksForTheCheckoutAndAppliesIt(t *testing.T) {
	dir := glueopsPins(t, "  - name: platform_crds_version\n    version: v0.1.6\n")
	checkout := t.TempDir()
	r := newMenuRig(t, dir, "crds", "custom", checkout, "Exit")
	r.rc["crds apply --chart-dir "+ui.CleanPath(checkout)] = 1
	if code := r.run(); code != 0 {
		t.Fatalf("%d", code)
	}
	if len(r.runs) != 1 || r.runs[0] != "crds apply --chart-dir "+ui.CleanPath(checkout) {
		t.Fatalf("%v", r.runs)
	}
	for _, w := range []string{"custom applies UNRELEASED CRDs", "NOT applied"} {
		if !strings.Contains(r.errs.String(), w) {
			t.Fatalf("missing %q in %q", w, r.errs.String())
		}
	}
	// an empty path is "back"
	r = newMenuRig(t, dir, "crds", "custom", "", "Exit")
	if r.run(); len(r.runs) != 0 {
		t.Fatalf("empty path ran %v", r.runs)
	}
}

func TestMenuCRDsDeclinedIsSkippedNotAFailure(t *testing.T) {
	dir := glueopsPins(t, "  - name: platform_crds_version\n    version: v0.1.6\n")
	r := newMenuRig(t, dir, "crds", "v0.1.6", "Exit")
	r.rc["crds apply --version v0.1.6"] = ExitDeclined
	if code := r.run(); code != 0 {
		t.Fatalf("%d", code)
	}
	if strings.Contains(r.errs.String(), "NOT applied") || !strings.Contains(r.errs.String(), "platform-crds v0.1.6 skipped") {
		t.Fatalf("a declined apply is not a failure: %q", r.errs.String())
	}
}

func TestMenuCodespaceVersionMismatchAsksFirst(t *testing.T) {
	dir := glueopsPins(t, "  - name: codespace_version\n    version: v1.2.3\n")
	for _, tc := range []struct {
		name, have string
		answers    []string
		code       int
		asked      bool
	}{
		{"match", "v1.2.3", []string{"Exit"}, 0, false},
		{"mismatch, go on", "v1.0.0", []string{"yes", "Exit"}, 0, true},
		{"mismatch, decline", "v1.0.0", []string{"no"}, 1, true},
		{"not in a codespace", "", []string{"Exit"}, 0, false},
	} {
		r := newMenuRig(t, dir, tc.answers...)
		r.m.getenv = func(string) string { return tc.have }
		code := r.run()
		got := len(r.prompt.Asked) > 0 && r.prompt.Asked[0] == "CONFIRM Confirmation"
		if code != tc.code || got != tc.asked {
			t.Fatalf("%s: exit %d, confirm asked %v (%v)", tc.name, code, got, r.prompt.Asked)
		}
		if tc.asked && !strings.Contains(r.errs.String(), "doesn't match with the desired: v1.2.3") {
			t.Fatalf("%s: %q", tc.name, r.errs.String())
		}
		if tc.code == 1 && len(r.prompt.Asked) != 1 {
			t.Fatalf("%s: the menu went on after a decline: %v", tc.name, r.prompt.Asked)
		}
	}
}

const preflightManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: not-a-set
---
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: tenants
---
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: platform-apps
`

func TestMenuPreflightMissingBinary(t *testing.T) {
	r := newMenuRig(t, t.TempDir(), "app_chart_preflight", "Exit")
	r.m.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if code := r.run(); code != 0 || len(r.pre) != 0 || !strings.Contains(r.errs.String(), "app-chart-preflight is not installed") {
		t.Fatalf("%d %v %q", code, r.pre, r.errs.String())
	}
}

func TestMenuPreflightAllSetsAndOneSet(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "manifests/manifest.yaml"), preflightManifest)

	r := newMenuRig(t, dir, "app_chart_preflight", " 0.1 8.0 ", "all", "Exit")
	r.m.getenv = func(k string) string {
		if k == "CAPTAIN_UTILS_PREFLIGHT" {
			return "/opt/preflight"
		}
		return ""
	}
	if code := r.run(); code != 0 {
		t.Fatalf("%d", code)
	}
	if len(r.pre) != 1 || strings.Join(r.pre[0], " ") != "/opt/preflight --target 0.18.0" {
		t.Fatalf("%v", r.pre)
	}
	if got := r.prompt.Asked[2]; got != "CHOOSE  [all|tenants|platform-apps|Back]" {
		t.Fatalf("set chooser: %q", got)
	}
	if !strings.Contains(r.errs.String(), `Preflight ok: bump only the sets under "Safe to bump"`) {
		t.Fatalf("%q", r.errs.String())
	}

	r = newMenuRig(t, dir, "app_chart_preflight", "v0.18.0", "tenants", "Exit")
	r.run()
	if len(r.pre) != 1 || strings.Join(r.pre[0], " ") != "app-chart-preflight --target v0.18.0 --appset tenants" {
		t.Fatalf("%v", r.pre)
	}
	if !strings.Contains(r.errs.String(), "Preflight ok: safe to bump tenants to v0.18.0.") {
		t.Fatalf("%q", r.errs.String())
	}
}

func TestMenuPreflightLooksAtTheSameClusterAsTheOtherItems(t *testing.T) {
	r := newMenuRig(t, t.TempDir(), "app_chart_preflight", "0.18.0", "all", "Exit")
	r.m.a.v.Set("kube-context", "k3d-foobar")
	r.run()
	if len(r.pre) != 1 || strings.Join(r.pre[0], " ") != "app-chart-preflight --target 0.18.0 --context k3d-foobar" {
		t.Fatalf("%v", r.pre)
	}
}

func TestMenuPreflightVerdicts(t *testing.T) {
	for rc, want := range map[int]string{1: "do NOT bump the sets it names", 2: "no verdict for some sets (exit 2)", 124: "(exit 124)"} {
		r := newMenuRig(t, t.TempDir(), "app_chart_preflight", "1.0.0", "all", "Exit")
		r.preRC = rc
		if code := r.run(); code != 0 || !strings.Contains(r.errs.String(), want) {
			t.Fatalf("rc %d: exit %d %q", rc, code, r.errs.String())
		}
	}
}

func TestMenuPreflightBackOutsRunNothing(t *testing.T) {
	for _, answers := range [][]string{
		{"app_chart_preflight", "   ", "Exit"},           // empty target
		{"app_chart_preflight", "1.0.0", "Back", "Exit"}, // Back at the set chooser
	} {
		r := newMenuRig(t, t.TempDir(), answers...)
		if code := r.run(); code != 0 || len(r.pre) != 0 {
			t.Fatalf("%v: %d %v", answers, code, r.pre)
		}
	}
}

func TestMenuItemsInheritTheGlobalFlags(t *testing.T) {
	r := newMenuRig(t, "/captain", "Exit")
	r.m.a.v.Set("kube-context", "k3d-foobar")
	r.m.a.yes = true
	r.m.a.v.Set("no-color", true)
	got := strings.Join(r.m.globalArgs(), " ")
	if got != "--captain-dir /captain --kube-context k3d-foobar --yes --no-color" {
		t.Fatal(got)
	}
	got = strings.Join(newMenuRig(t, ".", "Exit").m.globalArgs(), " ")
	if got != "--captain-dir ." {
		t.Fatal(got)
	}
}

func TestMenuArgsOnlyTheCaptainUtilsNameWithNoArgumentsOpensTheMenu(t *testing.T) {
	for _, tc := range []struct {
		argv0 string
		args  []string
		want  string
	}{
		{"/usr/local/bin/captain_utils", nil, "menu"},
		{"captain_utils", nil, "menu"},
		{"captain_utils", []string{"crds", "check"}, "crds check"},
		{"/usr/local/bin/captain-utils", nil, ""},
		{"captain-utils", []string{"menu"}, "menu"},
	} {
		if got := strings.Join(menuArgs(tc.argv0, tc.args), " "); got != tc.want {
			t.Fatalf("%s %v: %q, want %q", tc.argv0, tc.args, got, tc.want)
		}
	}
}

// A CAPTAIN_UTILS_YES or CRDS_AUTO_CONFIRM left in the shell neither skips a confirm nor
// leaks into the items the menu runs; the menu still opens on the item list.
func TestMenuCommandIgnoresApprovalEnvironmentVariables(t *testing.T) {
	t.Setenv("CAPTAIN_UTILS_YES", "true")
	t.Setenv("CRDS_AUTO_CONFIRM", "yes")
	var out, errb bytes.Buffer
	a := NewApp("v0.0.0-test", "abc", &out, &errb)
	a.Prompt = &menuPrompt{Script: ui.Script{Answers: []string{"Exit"}}}
	if code := a.Execute(context.Background(), []string{"--captain-dir", t.TempDir(), "menu"}); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "Goodbye!") {
		t.Fatalf("%q", out.String())
	}
	if a.opts().Yes || strings.Contains(strings.Join(a.newMenu().globalArgs(), " "), "--yes") {
		t.Fatal("an environment variable approved confirmations")
	}
}

// The real runSubcommand: a menu item runs the real command tree through a child App,
// with the menu's own settings and a prompter and cluster shared with it.
func TestMenuRunsRealSubcommandsThroughAChildApp(t *testing.T) {
	var out, errb bytes.Buffer
	a := NewApp("v0.0.0-test", "abc", &out, &errb)
	a.NewCluster = func(string) (Cluster, error) { return fakeCluster{}, nil }
	a.Prompt = &menuPrompt{}
	a.Root()
	a.p = ui.New(&out, &errb, true)
	m := a.newMenu()
	if code := m.run(context.Background(), "pods", "watch", "--once"); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "broken") {
		t.Fatalf("%q", out.String())
	}
	// the child reports its own failures with its own exit code, and the parent app is untouched
	if code := m.run(context.Background(), "crds", "bogus"); code != ExitUsage || !strings.Contains(errb.String(), "unknown command") {
		t.Fatalf("%d %q", code, errb.String())
	}
}

// Through the whole stack: the real command opens on the item list; Exit leaves it.
func TestMenuEndToEndOpensOnTheItemsThenExit(t *testing.T) {
	var out, errb bytes.Buffer
	a := NewApp("v0.0.0-test", "abc", &out, &errb)
	a.NewCluster = func(string) (Cluster, error) { return fakeCluster{}, nil }
	a.Prompt = &menuPrompt{Script: ui.Script{Answers: []string{"Exit"}}}
	if code := a.Execute(context.Background(), []string{"--captain-dir", t.TempDir(), "menu"}); code != 0 || !strings.Contains(out.String(), "Goodbye!") {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
}
