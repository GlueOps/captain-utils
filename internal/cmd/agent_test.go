package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aws main.tf commands with -o json: stdout is only the result, the diff is inside it, and
// --dry-run leaves main.tf alone.
func TestAWSKubernetesJSON(t *testing.T) {
	dir := captainDir(t)
	tf := filepath.Join(dir, "terraform/kubernetes/main.tf")

	code, out, _ := run(t, nil, nil, "aws", "kubernetes", "--dry-run", "-o", "json", "--captain-dir", dir)
	var res awsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("stdout is not JSON: %v %q", err, out)
	}
	if code != 0 || !res.Changed || !res.DryRun || res.Written || len(res.Changes) != 1 ||
		res.Changes[0].Key != "eks_version" || res.Changes[0].Old != "1.33" || res.Changes[0].New != "1.34" ||
		!strings.Contains(res.Diff, `+  eks_version        = "1.34"`) || res.Error != "" {
		t.Fatalf("exit %d %+v", code, res)
	}
	if b, _ := os.ReadFile(tf); strings.Contains(string(b), `"1.34"`) {
		t.Fatal("--dry-run wrote main.tf")
	}

	code, out, _ = run(t, nil, nil, "aws", "kubernetes", "-o", "json", "--captain-dir", dir)
	res = awsResult{}
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 0 || !res.Written {
		t.Fatalf("exit %d %v %q", code, err, out)
	}

	// nothing left to change: still one JSON object, changes is [] not null
	code, out, _ = run(t, nil, nil, "aws", "kubernetes", "-o", "json", "--captain-dir", dir)
	if code != 0 || !strings.Contains(out, `"changes": []`) || !strings.Contains(out, `"changed": false`) {
		t.Fatalf("exit %d %q", code, out)
	}
}

// A failure still answers on stdout (error field), with the usual exit code.
func TestAWSJSONFailure(t *testing.T) {
	code, out, _ := run(t, nil, nil, "aws", "kubernetes", "-o", "json", "--captain-dir", t.TempDir())
	var res awsResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 1 || res.Error == "" {
		t.Fatalf("exit %d %v %q", code, err, out)
	}
	if code, out, _ = run(t, nil, nil, "aws", "kubernetes", "-o", "yaml", "--captain-dir", t.TempDir()); code != ExitUsage || out != "" {
		t.Fatalf("exit %d %q", code, out)
	}
}

// pods list is a one-shot snapshot an agent can parse; --all includes healthy pods.
func TestPodsListJSON(t *testing.T) {
	code, out, _ := run(t, fakeCluster{}, nil, "pods", "list", "-o", "json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || code != 0 || len(rows) == 0 {
		t.Fatalf("exit %d %v %q", code, err, out)
	}
	for _, k := range []string{"namespace", "name", "ready", "status", "restarts", "age"} {
		if _, ok := rows[0][k]; !ok {
			t.Fatalf("no %q in %v", k, rows[0])
		}
	}
	_, all, _ := run(t, fakeCluster{}, nil, "pods", "list", "-o", "json", "--all")
	var allRows []map[string]any
	if err := json.Unmarshal([]byte(all), &allRows); err != nil || len(allRows) <= len(rows) {
		t.Fatalf("--all gave %d rows, default %d (%v)", len(allRows), len(rows), err)
	}
	if code, out, _ := run(t, fakeCluster{}, nil, "pods", "list"); code != 0 || !strings.HasPrefix(out, "NAMESPACE") || strings.Contains(out, "Every") {
		t.Fatalf("text form: exit %d %q", code, out)
	}
	if code, _, _ := run(t, fakeCluster{}, nil, "pods", "list", "-o", "yaml"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

// -o text is the human form everywhere, versions diff included.
func TestVersionsDiffAcceptsText(t *testing.T) {
	dir := fullCaptain(t)
	if code, out, errs := run(t, fakeCluster{}, nil, "versions", "diff", "-o", "text", "--captain-dir", dir); code != 0 && !strings.Contains(out, "Tool Name") {
		t.Fatalf("exit %d %q %q", code, out, errs)
	}
	if code, _, _ := run(t, fakeCluster{}, nil, "versions", "diff", "-o", "yaml", "--captain-dir", dir); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

// A command line cobra rejects still answers in JSON when JSON was asked for.
func TestUsageErrorJSON(t *testing.T) {
	code, out, _ := run(t, fakeCluster{}, nil, "crds", "check", "--bogus", "-o", "json")
	var res map[string]string
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != ExitUsage || !strings.Contains(res["error"], "bogus") {
		t.Fatalf("exit %d %v %q", code, err, out)
	}
	// no JSON requested: stdout stays empty
	if code, out, _ = run(t, fakeCluster{}, nil, "crds", "check", "--bogus"); code != ExitUsage || out != "" {
		t.Fatalf("exit %d %q", code, out)
	}
}
