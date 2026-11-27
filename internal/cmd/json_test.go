package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/glueops/captain-utils/internal/crds"
	"github.com/glueops/captain-utils/internal/upgrade"
)

// crds check -o json: stdout is only the report, with the same exit code as the text form.
func TestCRDsCheckJSON(t *testing.T) {
	dir := fullCaptain(t)
	// the bundle's CRD is not on the (fake, empty) cluster: drift, exit 1
	code, out, errs, _ := runCLI(t, dir, nil, false, "crds", "check", "--version", "v0.1.6", "-o", "json")
	var rep crds.CheckReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("stdout is not JSON: %v %q", err, out)
	}
	if code != 1 || rep.InSync || rep.Version != "v0.1.6" || rep.New != 1 || rep.Total != 1 || rep.NotOnCluster != 1 ||
		len(rep.Differs) != 1 || len(rep.Reasons) != 1 || rep.Error != "" {
		t.Fatalf("exit %d %+v", code, rep)
	}
	// the plan and the drift verdict stay on stderr, not in the JSON
	if !strings.Contains(errs, "1 new, 0 changed") || strings.Contains(out, "1 new") {
		t.Fatalf("stderr %q stdout %q", errs, out)
	}
	// slices are [] not null, so a script can iterate them
	if !strings.Contains(out, `"terminating": []`) || !strings.Contains(out, `"notOwned": []`) {
		t.Fatalf("nil slice in %s", out)
	}
}

// A failed check still answers on stdout (error field) and exits 2, never 1 (drift).
func TestCRDsCheckJSONFailure(t *testing.T) {
	dir := captainDir(t) // no platform.yaml: the preflight fails
	code, out, _, _ := runCLI(t, dir, nil, false, "crds", "check", "--version", "v0.1.6", "-o", "json")
	var rep crds.CheckReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 2 || rep.InSync || !strings.Contains(rep.Error, "platform.yaml") {
		t.Fatalf("exit %d %v %q", code, err, out)
	}
}

func TestOutputFlagRejectsOtherFormats(t *testing.T) {
	dir := fullCaptain(t)
	for _, args := range [][]string{
		{"crds", "check", "--version", "v0.1.6", "-o", "yaml"},
		{"platform", "diff", "--version", "v0.85.0", "-o", "yaml"},
		{"argocd", "upgrade", "--version", "9.3.7", "-o", "yaml", "--yes"},
		{"aws", "calico", "upgrade", "-o", "yaml"},
	} {
		code, out, _, up := runCLI(t, dir, nil, false, args...)
		if code != ExitUsage || out != "" || up {
			t.Errorf("%v: exit %d out %q upgraded %v", args, code, out, up)
		}
	}
}

func runResult(t *testing.T, dir string, answers []string, tty bool, args ...string) (int, upgrade.Result, string, string) {
	t.Helper()
	code, out, errs, _ := runCLI(t, dir, answers, tty, append(args, "-o", "json")...)
	var res upgrade.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: stdout is not one JSON object: %v %q (stderr %q)", args, err, out, errs)
	}
	return code, res, out, errs
}

// diff -o json: the helm diff is the "diff" field, and nothing else reaches stdout.
func TestDiffJSON(t *testing.T) {
	dir := fullCaptain(t)
	for _, tc := range []struct {
		args    []string
		release string
	}{
		{[]string{"platform", "diff", "--version", "v0.85.0"}, "glueops-platform"},
		{[]string{"argocd", "diff", "--version", "9.3.7"}, "argocd"},
	} {
		code, res, out, errs := runResult(t, dir, nil, false, tc.args...)
		if code != 0 || res.Release != tc.release || res.Action != upgrade.ActionDiff || res.Changed == nil || !*res.Changed ||
			res.Diff != "changed\n" || res.Version == "" || res.Error != "" {
			t.Errorf("%v: exit %d %+v", tc.args, code, res)
		}
		if strings.Contains(out, "chosen version") || !strings.Contains(errs, "chosen version") {
			t.Errorf("%v: status text on stdout: %q / stderr %q", tc.args, out, errs)
		}
	}
}

func TestUpgradeJSON(t *testing.T) {
	dir := fullCaptain(t)
	for _, args := range [][]string{
		{"platform", "upgrade", "--version", "v0.85.0", "--yes"},
		{"argocd", "upgrade", "--version", "9.3.7", "--yes"},
		{"aws", "calico", "upgrade", "--yes"},
	} {
		code, res, out, _ := runResult(t, dir, nil, false, args...)
		if code != 0 || res.Action != upgrade.ActionUpgraded || res.Status != "deployed" || res.Revision != 2 ||
			res.Changed == nil || res.Release == "" || res.Namespace == "" || res.Error != "" {
			t.Errorf("%v: exit %d %+v", args, code, res)
		}
		if strings.Contains(out, "Happy Helming") {
			t.Errorf("%v: helm text on stdout: %q", args, out)
		}
	}
}

// A declined confirm is exit 3 and says so in the result: nothing was upgraded.
func TestUpgradeJSONDeclinedAndFailed(t *testing.T) {
	dir := fullCaptain(t)
	code, res, _, _ := runResult(t, dir, []string{"no"}, true, "platform", "upgrade", "--version", "v0.85.0")
	if code != ExitDeclined || res.Action != upgrade.ActionDeclined || res.Error == "" || res.Release != "glueops-platform" {
		t.Fatalf("declined: exit %d %+v", code, res)
	}
	// no --yes and no terminal: a usage error (exit 2) with the reason in the result, nothing upgraded
	code, res, _, _ = runResult(t, dir, nil, false, "argocd", "upgrade", "--version", "9.3.7")
	if code != ExitUsage || res.Action != upgrade.ActionNone || !strings.Contains(res.Error, "--yes") {
		t.Fatalf("failed: exit %d %+v", code, res)
	}
}

// A usage error (no terminal and no --version) still exits 2, and now also answers on stdout with the
// reason in "error", so a caller reads one stream for every outcome.
func TestUpgradeJSONUsageError(t *testing.T) {
	dir := fullCaptain(t)
	code, res, out, errs := runResult(t, dir, nil, false, "platform", "diff")
	if code != ExitUsage || res.Action != upgrade.ActionNone || !strings.Contains(res.Error, "--version") || !strings.Contains(errs, "--version") {
		t.Fatalf("exit %d %+v out %q err %q", code, res, out, errs)
	}
}

// crds check -o json: a usage error or a failure before the cluster was read has the same shape as a
// finding: every list is [] (never null) and "error" says why.
func TestCRDsCheckJSONEarlyFailureHasEmptyListsNotNull(t *testing.T) {
	dir := fullCaptain(t)
	for name, args := range map[string][]string{
		"usage (no --version, no terminal)": {"crds", "check", "-o", "json"},
		"bad version":                       {"crds", "check", "--version", "not-a-version", "-o", "json"},
	} {
		code, out, _, _ := runCLI(t, dir, nil, false, args...)
		var rep crds.CheckReport
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("%s: stdout is not JSON: %v %q", name, err, out)
		}
		if code != ExitUsage || rep.InSync || rep.Error == "" || strings.Contains(out, "null") {
			t.Errorf("%s: exit %d %q", name, code, out)
		}
		for _, l := range []string{"differs", "terminating", "notEstablished", "notOwned", "reasons"} {
			if !strings.Contains(out, `"`+l+`": []`) {
				t.Errorf("%s: %s is not []: %s", name, l, out)
			}
		}
	}
}

func runApplyJSON(t *testing.T, dir string, answers []string, tty bool, args ...string) (int, crds.Result, string, string) {
	t.Helper()
	code, out, errs, _ := runCLI(t, dir, answers, tty, append([]string{"crds", "apply", "-o", "json"}, args...)...)
	var res crds.Result
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: stdout is not one JSON object: %v %q (stderr %q)", args, err, out, errs)
	}
	return code, res, out, errs
}

// crds apply -o json always answers on stdout with "error" set when the run did not finish: a declined
// confirm is exit 3, a usage error exit 2, a failure before any CRD was looked at exit 1 or 2.
func TestCRDsApplyJSONErrorsAreInTheResult(t *testing.T) {
	dir := fullCaptain(t)
	// declined: the bundle's CRD is new, so apply asks, and the answer is no
	code, res, out, _ := runApplyJSON(t, dir, []string{"no"}, true, "--version", "v0.1.6")
	if code != ExitDeclined || res.Error == "" || len(res.Applied) != 0 || res.Version != "v0.1.6" || strings.Contains(out, "null") {
		t.Errorf("declined: exit %d %+v", code, res)
	}
	// no terminal and no --yes: a usage error, nothing applied
	code, res, _, errs := runApplyJSON(t, dir, nil, false, "--version", "v0.1.6")
	if code != ExitUsage || !strings.Contains(res.Error, "--yes") || len(res.Applied) != 0 || !strings.Contains(errs, "--yes") {
		t.Errorf("no --yes: exit %d %+v", code, res)
	}
	// usage: no --version and no terminal
	code, res, out, _ = runApplyJSON(t, dir, nil, false)
	if code != ExitUsage || !strings.Contains(res.Error, "--version") || strings.Contains(out, "null") {
		t.Errorf("usage: exit %d %+v", code, res)
	}
	// failure before any CRD: no platform.yaml
	code, res, _, _ = runApplyJSON(t, captainDir(t), nil, false, "--version", "v0.1.6", "--yes")
	if code != 1 || !strings.Contains(res.Error, "platform.yaml") {
		t.Errorf("early failure: exit %d %+v", code, res)
	}
}

// When the post-apply check fails (the stub cluster lists no CRDs, so "expected 1 bundle CRDs live"),
// exit is 1 and the result still lists what was applied next to the error: a script can see both.
func TestCRDsApplyJSONVerifyFailureKeepsWhatWasApplied(t *testing.T) {
	dir := fullCaptain(t)
	code, res, _, _ := runApplyJSON(t, dir, nil, false, "--version", "v0.1.6", "--yes")
	if code != 1 || len(res.Applied) != 1 || res.Applied[0] != "a.x.io" || len(res.Failed) != 0 ||
		!strings.Contains(res.Error, "expected 1 bundle CRDs live") || res.Established {
		t.Fatalf("exit %d %+v", code, res)
	}
}

// -o json on crds always has "error" (empty on success, as in the upgrade results), and --summary
// leaves the name lists out but keeps the counts.
func TestCRDsJSONErrorKeyAndSummary(t *testing.T) {
	dir := fullCaptain(t)
	_, out, _, _ := runCLI(t, dir, nil, false, "crds", "check", "--version", "v0.1.6", "-o", "json", "--summary")
	if !strings.Contains(out, `"error": ""`) || !strings.Contains(out, `"differs": []`) || !strings.Contains(out, `"new": 1`) {
		t.Fatalf("check: %s", out)
	}
	_, out, _, _ = runCLI(t, dir, nil, false, "crds", "apply", "--version", "v0.1.6", "--yes", "-o", "json")
	// the fake cluster keeps nothing, so this run ends in an error: the key is there either way
	if !strings.Contains(out, `"error": "`) || !strings.Contains(out, `"appliedCount": 1`) {
		t.Fatalf("apply: %s", out)
	}
	_, out, _, _ = runCLI(t, dir, nil, false, "crds", "apply", "--version", "v0.1.6", "--yes", "-o", "json", "--summary")
	if strings.Contains(out, "x.io") || !strings.Contains(out, `"appliedCount": 1`) {
		t.Fatalf("apply --summary: %s", out)
	}
	if code, out, _, _ := runCLI(t, dir, nil, false, "crds", "check", "--version", "v0.1.6", "--summary"); code != ExitUsage || out != "" {
		t.Fatalf("--summary without -o json: exit %d %q", code, out)
	}
}

// An early failure still carries every documented key (version, diff, status ...), never fewer.
func TestUpgradeJSONAlwaysHasEveryKey(t *testing.T) {
	_, _, out, _ := runResult(t, fullCaptain(t), nil, false, "argocd", "upgrade", "--version", "9.3.7")
	for _, k := range []string{`"release"`, `"namespace"`, `"chart"`, `"version"`, `"action"`, `"changed"`, `"diff"`, `"diffLines"`, `"status"`, `"revision"`, `"notes"`, `"error"`} {
		if !strings.Contains(out, k) {
			t.Errorf("key %s missing from %s", k, out)
		}
	}
}

// --no-diff keeps the diff text out of the result; changed and diffLines still say what changed.
func TestDiffJSONNoDiff(t *testing.T) {
	dir := fullCaptain(t)
	for _, args := range [][]string{
		{"platform", "diff", "--version", "v0.85.0", "--no-diff"},
		{"argocd", "diff", "--version", "9.3.7", "--no-diff"},
	} {
		code, res, out, _ := runResult(t, dir, nil, false, args...)
		if code != 0 || res.Diff != "" || res.DiffLines != 1 || res.Changed == nil || !*res.Changed || !strings.Contains(out, `"diff": ""`) {
			t.Errorf("%v: exit %d %+v", args, code, res)
		}
	}
	// without --no-diff the text and the count are both there
	_, res, _, _ := runResult(t, dir, nil, false, "platform", "diff", "--version", "v0.85.0")
	if res.Diff != "changed\n" || res.DiffLines != 1 {
		t.Errorf("%+v", res)
	}
	// the upgrade result honours it too
	code, res, _, _ := runResult(t, dir, nil, false, "argocd", "upgrade", "--version", "9.3.7", "--yes", "--no-diff")
	if code != 0 || res.Action != upgrade.ActionUpgraded || res.Diff != "" || res.DiffLines != 1 {
		t.Errorf("upgrade: exit %d %+v", code, res)
	}
}

// --no-diff only means something with -o json; in text mode it is a usage error, not silently ignored.
func TestNoDiffNeedsJSON(t *testing.T) {
	dir := fullCaptain(t)
	code, out, errs, up := runCLI(t, dir, nil, false, "platform", "diff", "--version", "v0.85.0", "--no-diff")
	if code != ExitUsage || out != "" || up || !strings.Contains(errs, "-o json") {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
}
