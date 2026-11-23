package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glueops/captain-utils/internal/crds"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/ui"
)

type noBundle struct{}

func (noBundle) CRDBundle(context.Context, string, string, string) (*helmx.CRDBundle, error) {
	return &helmx.CRDBundle{Manifest: []byte("---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: a.x.io\n"), Union: 1}, nil
}

func runCRDs(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("CAPTAIN_UTILS_CAPTAIN_DIR", dir)
	return runWith(t, func(a *App) {}, args...)
}

type noTerminal struct{ ui.Script }

func (noTerminal) Interactive() bool { return false }

func runWith(t *testing.T, set func(*App), args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	a := NewApp("v0.0.0-test", "abc", &out, &errb)
	a.NewCluster = func(string) (Cluster, error) { return fakeCluster{server: "1.34"}, nil }
	a.NewCRDSource = func(string) crds.Source { return noBundle{} }
	a.Prompt = &ui.Script{}
	set(a)
	code := a.Execute(context.Background(), args)
	return code, out.String(), errb.String()
}

func TestCRDsCheckExitCodes(t *testing.T) {
	dir := captainDir(t)
	// no platform_crds_version pin: a failure, exit 2, not drift
	code, _, errs := runCRDs(t, dir, "crds", "check")
	if code != 2 || !strings.Contains(errs, "platform_crds_version") {
		t.Fatalf("%d %q", code, errs)
	}
	// the bundle's CRD is missing from the (fake, empty) cluster: drift, exit 1, nothing else printed
	code, out, errs := runCRDs(t, dir, "crds", "check", "--version", "v0.1.6")
	if code != 2 || !strings.Contains(errs, "no platform.yaml") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	mustWrite(t, dir+"/platform.yaml", "a: 1\n")
	code, out, _ = runCRDs(t, dir, "crds", "check", "--version", "v0.1.6")
	if code != 1 || !strings.Contains(out, "1 new, 0 changed") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCRDsVersionAndChartDirAreExclusive(t *testing.T) {
	code, _, errs := runCRDs(t, captainDir(t), "crds", "apply", "--version", "v0.1.6", "--chart-dir", "x")
	if code == 0 || !strings.Contains(errs, "none of the others can be") && !strings.Contains(errs, "if any flags in the group") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestCRDsApplyWithoutTerminalNeedsYes(t *testing.T) {
	dir := captainDir(t)
	mustWrite(t, dir+"/platform.yaml", "a: 1\n")
	mustWrite(t, dir+"/VERSIONS/glueops.yaml", "versions:\n  - name: platform_crds_version\n    version: v0.1.6\n")
	code, _, errs := runWith(t, func(a *App) {
		a.Prompt = &noTerminal{}
	}, "--captain-dir", dir, "crds", "apply")
	if code == 0 || !strings.Contains(errs, "--yes") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestInterruptedRunExitsWith130(t *testing.T) {
	dir := captainDir(t)
	mustWrite(t, dir+"/platform.yaml", "a: 1\n")
	var out, errb bytes.Buffer
	a := NewApp("v0.0.0-test", "abc", &out, &errb)
	a.NewCluster = func(string) (Cluster, error) { return fakeCluster{}, nil }
	a.NewCRDSource = func(string) crds.Source { return cancelledSource{} }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := a.Execute(ctx, []string{"--captain-dir", dir, "crds", "check", "--version", "v0.1.6"}); code != 130 || !strings.Contains(errb.String(), "interrupted") {
		t.Fatalf("%d %q", code, errb.String())
	}
}

type cancelledSource struct{}

func (cancelledSource) CRDBundle(ctx context.Context, _, _, _ string) (*helmx.CRDBundle, error) {
	return nil, ctx.Err()
}

func TestUnknownSubcommandOfAGroupFailsInsteadOfPrintingHelp(t *testing.T) {
	// regression: `crds apply-dir DIR` (the bash command's name) exited 0 and did nothing
	for _, args := range [][]string{{"crds", "apply-dir", "x"}, {"platform", "bogus"}, {"aws", "calico", "bogus"}} {
		code, _, errs := run(t, fakeCluster{}, nil, args...)
		if code == 0 || !strings.Contains(errs, "unknown command") {
			t.Fatalf("%v: %d %q", args, code, errs)
		}
	}
	// the group on its own still prints help
	if code, out, _ := run(t, fakeCluster{}, nil, "crds"); code != 0 || !strings.Contains(out, "Available Commands") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestCRDsWaitTimeoutEnvBareNumberIsSeconds(t *testing.T) {
	// regression: the bash command took CRDS_WAIT_TIMEOUT in seconds; GetDuration read "300" as 300ns,
	// so every wait timed out at once
	for env, want := range map[string]time.Duration{
		"300": 300 * time.Second, "90s": 90 * time.Second, "2m": 2 * time.Minute, "": crds.DefaultWait,
	} {
		t.Setenv("CRDS_WAIT_TIMEOUT", env)
		a := NewApp("v0.0.0-test", "abc", &bytes.Buffer{}, &bytes.Buffer{})
		a.NewCluster = func(string) (Cluster, error) { return fakeCluster{}, nil }
		root := a.Root()
		if err := root.ParseFlags(nil); err != nil {
			t.Fatal(err)
		}
		d, err := a.crdsDeps()
		if err != nil || d.Wait != want {
			t.Fatalf("CRDS_WAIT_TIMEOUT=%q: %v %v, want %v", env, d, err, want)
		}
	}
}

func TestCRDsCheckHasRequireOwnedFlag(t *testing.T) {
	code, out, _ := runCRDs(t, captainDir(t), "crds", "check", "--help")
	if code != 0 || !strings.Contains(out, "--require-owned") {
		t.Fatalf("%d %q", code, out)
	}
}
