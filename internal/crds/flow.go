package crds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/kube"
	"github.com/glueops/captain-utils/internal/ui"
)

// Source renders the CRD bundle (helmx.Client).
type Source interface {
	CRDBundle(ctx context.Context, chartRef, version, valuesFile string) (*helmx.CRDBundle, error)
}

// Cluster is what the flows read from and do to the cluster (kube.Client).
type Cluster interface {
	ListCRDs(ctx context.Context) ([]*unstructured.Unstructured, error)
	Apply(ctx context.Context, obj *unstructured.Unstructured, o kube.ApplyOptions) (*unstructured.Unstructured, error)
	RemoveCRDAnnotation(ctx context.Context, name, key string) error
	RemoveCRDLabel(ctx context.Context, name, key string) error
	MigrateCRDToSSA(ctx context.Context, name, manager string) (bool, error)
	WaitCRDsDeleted(ctx context.Context, names []string, timeout time.Duration) error
	WaitCRDsEstablished(ctx context.Context, names []string, timeout time.Duration) error
}

// Releases lists GitHub release tags (dev mode's version list).
type Releases interface {
	ReleaseTags(ctx context.Context, repo string, n int) ([]string, error)
}

// Deps is everything the flows use.
type Deps struct {
	Source  Source
	Cluster Cluster
	GitHub  Releases
	Prompt  ui.Prompter
	P       *ui.Printer
	Out     io.Writer // plan, diff and verdict (data); status lines go through P
	Repo    captain.Repo
	Env     string // production | dev
	Yes     bool   // --yes: confirmations answer yes
	Paged   bool   // show the diff in the pager (stdout is a terminal)
	// Chart overrides the chart reference (CRDS_CHART).
	Chart string
	// Wait bounds both waits (CRDS_WAIT_TIMEOUT).
	Wait        time.Duration
	GitInfo     func(dir string) string
	PathPrefill string
}

func (d *Deps) production() bool { return d.Env != "dev" }

func (d *Deps) chart() string {
	if d.Chart != "" {
		return d.Chart
	}
	return DefaultChart
}

func (d *Deps) wait() time.Duration {
	if d.Wait > 0 {
		return d.Wait
	}
	return DefaultWait
}

// Options select the bundle without asking.
type Options struct {
	Version  string // a release tag; empty = the pin (production) or a chooser (dev apply)
	ChartDir string // a local platform-crds checkout
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// pinned is the platform_crds_version VERSIONS/glueops.yaml declares (exactly one).
func (d *Deps) pinned() (string, error) {
	pins, err := d.Repo.GlueOpsVersions()
	if err != nil {
		return "", fmt.Errorf("cannot read platform_crds_version from VERSIONS/glueops.yaml — run from the captain repo root, or pass --version: %w", err)
	}
	all := pins.All(captain.PlatformCRDsVersion)
	if len(all) == 0 {
		return "", fmt.Errorf("%s has no %s pin", pins.File, captain.PlatformCRDsVersion)
	}
	if len(all) > 1 {
		return "", fmt.Errorf("%s has %d %s pins, want exactly one", pins.File, len(all), captain.PlatformCRDsVersion)
	}
	return all[0], nil
}

// fetch renders and validates the bundle for version (localVersion: from dir).
func (d *Deps) fetch(ctx context.Context, version, dir string) (*bundle, error) {
	ref, ver := d.chart(), strings.TrimPrefix(version, "v")
	if dir != "" {
		ref, ver = dir, ""
	}
	values := d.Repo.Path("platform.yaml")
	switch {
	case captain.Exists(values):
	case d.production():
		return nil, fmt.Errorf("no platform.yaml in %s — the bundle reads it to pick the CRD profiles for this cluster; run from the captain repo root", d.Repo.Dir)
	default:
		d.P.Warn("no platform.yaml (dev): installing every profile")
		values = ""
	}
	if dir == "" {
		d.P.Note("bundle source: %s --version %s", ref, ver)
	}
	cb, err := d.Source.CRDBundle(ctx, ref, ver, values)
	if err != nil {
		return nil, fmt.Errorf("platform-crds %s: %w", version, err)
	}
	return parseBundle(version, cb)
}

// diff is `kubectl diff --server-side --force-conflicts`: each bundle CRD is
// dry-run applied and compared with the live one. A CRD that does not exist
// yet diffs from an empty file.
func (d *Deps) diff(ctx context.Context, b *bundle, live []*unstructured.Unstructured) (diffResult, error) {
	byName := map[string]*unstructured.Unstructured{}
	for _, o := range live {
		byName[o.GetName()] = o
	}
	var res diffResult
	var text strings.Builder
	for _, obj := range b.objs {
		merged, err := d.Cluster.Apply(ctx, obj, kube.ApplyOptions{FieldManager: FieldManager, Force: true, DryRun: true})
		if err != nil {
			return res, fmt.Errorf("diff %s: %w", obj.GetName(), err)
		}
		cur := byName[obj.GetName()]
		var from string
		if cur != nil {
			from = normalize(cur)
		}
		to := normalize(merged)
		if from == to {
			continue
		}
		name := "apiextensions.k8s.io.v1.CustomResourceDefinition.." + obj.GetName()
		u, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(from), B: difflib.SplitLines(to),
			FromFile: "LIVE/" + name, ToFile: "MERGED/" + name, Context: 3,
		})
		if err != nil {
			return res, err
		}
		fmt.Fprintf(&text, "diff -u -N LIVE/%s MERGED/%s\n%s", name, name, u)
		if cur == nil {
			res.New++
		} else {
			res.Changed++
		}
	}
	res.Text = text.String()
	return res, nil
}

func count(s []string) int { return len(s) }

// plan is everything this run will do, printed before the confirm and
// unconditionally: the no-diff branch has no confirm yet still strips ArgoCD
// tracking, so a plan inside the changed branch would hide precisely the run
// whose only effect is a mutation.
func (d *Deps) plan(version string, b *bundle, live []*unstructured.Unstructured, df diffResult, apply bool) {
	ann, lab, dying := argoTrackedAnnotation(live), argoTrackedLabel(live), terminating(live)
	d.P.Highlight("platform-crds %s — %s", version, b.scope())
	if df.drift() {
		fmt.Fprintf(d.Out, "  %d new, %d changed\n", df.New, df.Changed)
	} else {
		fmt.Fprintln(d.Out, "  apply:  no content changes")
	}
	if !apply {
		return // check changes nothing: strip, wait and keep describe an apply
	}
	if n := csaManaged(live); n > 0 {
		fmt.Fprintf(d.Out, "  migrate: client-side-apply ownership of %d CRD(s) moves to %s (last-applied-configuration is dropped; the diff above does not show it)\n", n, FieldManager)
	}
	fmt.Fprintf(d.Out, "  strip:  ArgoCD tracking-id from %d, instance label from %d\n", count(ann), count(lab))
	// An observation, not a promise: the documented crds -> argocd -> crds order means
	// the argocd release is itself what creates a terminating CRD.
	if len(dying) > 0 {
		fmt.Fprintf(d.Out, "  wait:   %d terminating, the apply waits up to %s: %s\n", len(dying), secs(d.wait()), join(dying))
	} else {
		fmt.Fprintln(d.Out, "  wait:   none terminating right now — re-checked after you confirm, a wait may still follow")
	}
	fmt.Fprintln(d.Out, "  keep:   chart labels an earlier installer set are NOT removed (the bundle never declares them)")
}

// clearance is the token doApply demands. Only clear makes one, and it is bound
// to the snapshot it cleared: a deleted guard call does not compile, a stale
// snapshot is rejected.
type clearance struct {
	ok   bool
	live []*unstructured.Unstructured
}

// clear is the mutating pre-apply sequence: strip stray ArgoCD tracking metadata
// (a tracked CRD would be auto-pruned once it leaves an app's desired state)
// and wait out CRDs that are being deleted (the Gate CRD right after the argocd
// release dropped it must be gone before it is re-created).
// kubectl.kubernetes.io/last-applied-configuration is deliberately NOT stripped:
// server-side apply uses it to migrate client-side-apply ownership.
func (d *Deps) clear(ctx context.Context, live []*unstructured.Unstructured) (clearance, error) {
	if ann := argoTrackedAnnotation(live); len(ann) > 0 {
		d.P.Info("Removing ArgoCD tracking annotation from: %s", join(ann))
		for _, n := range ann {
			if err := d.Cluster.RemoveCRDAnnotation(ctx, n, argoTrackingID); err != nil {
				return clearance{}, err
			}
		}
	}
	if lab := argoTrackedLabel(live); len(lab) > 0 {
		d.P.Info("Removing ArgoCD instance label from: %s", join(lab))
		for _, n := range lab {
			if err := d.Cluster.RemoveCRDLabel(ctx, n, argoInstance); err != nil {
				return clearance{}, err
			}
		}
	}
	if dying := terminating(live); len(dying) > 0 {
		d.P.Info("Waiting for terminating CRD(s) to disappear: %s", join(dying))
		if err := d.Cluster.WaitCRDsDeleted(ctx, sortedCopy(dying), d.wait()); err != nil {
			var to *kube.TimeoutError
			if errors.As(err, &to) {
				d.P.Plain("   a custom resource with a finalizer is holding the type open; clear them, then re-run")
				return clearance{}, fmt.Errorf("still terminating after %s — the bundle was NOT applied: %s", secs(d.wait()), join(to.Pending))
			}
			return clearance{}, fmt.Errorf("waiting for terminating CRDs — the bundle was NOT applied: %w", err)
		}
	}
	return clearance{ok: true, live: live}, nil
}

// doApply is the only mutating Apply in this package, and it cannot run without
// a clearance. It applies every CRD and reports all failures, as kubectl apply does.
func (d *Deps) doApply(ctx context.Context, b *bundle, c clearance) error {
	if !c.ok {
		return errors.New("BUG: doApply called without a clearance")
	}
	d.P.Note("+ server-side apply of %d CRDs (force-conflicts, field manager %s)", b.total(), FieldManager)
	var errs []error
	for _, obj := range b.objs {
		// what kubectl apply --server-side does first: hand client-side-apply ownership (and with
		// it the last-applied annotation) over to our manager, so the apply below can drop them
		if _, err := d.Cluster.MigrateCRDToSSA(ctx, obj.GetName(), FieldManager); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := d.Cluster.Apply(ctx, obj, kube.ApplyOptions{FieldManager: FieldManager, Force: true}); err != nil {
			errs = append(errs, fmt.Errorf("apply %s: %w", obj.GetName(), err))
		}
	}
	return errors.Join(errs...)
}

func (d *Deps) confirm(title string) (bool, error) {
	if d.Yes {
		d.P.Note("%s yes (--yes)", title)
		return true, nil
	}
	if !d.Prompt.Interactive() {
		return false, fmt.Errorf("%s: %w (pass --yes to run without one)", title, ui.ErrNoTerminal)
	}
	return d.Prompt.Confirm(title)
}

func (d *Deps) preflight(ctx context.Context, b *bundle) ([]*unstructured.Unstructured, error) {
	live, err := readLive(ctx, d.Cluster, b)
	if err != nil {
		return nil, err
	}
	errs, warns := storedVersions(b, live)
	if len(errs) > 0 {
		d.P.Alert("❌ storage-version migration required before this bundle can be applied:")
		for _, e := range errs {
			d.P.Plain("  %s", e)
		}
		d.P.Plain("  fix (per CRD):  kubectl get <plural.group> -A -o json | kubectl replace -f -   # rewrites objects at the current storage version")
		d.P.Plain("                  kubectl patch crd <name> --subresource=status --type=merge -p '{\"status\":{\"storedVersions\":[\"<remaining>\"]}}'")
		return nil, errors.New("storage-version migration required")
	}
	for _, w := range warns {
		d.P.Warn("  ⚠ %s", w)
	}
	return live, nil
}

// target is what to render: a version, or a local checkout.
type target struct{ version, dir string }

func (t target) label() string {
	if t.dir != "" {
		return localVersion
	}
	return t.version
}

// resolveApply chooses what to apply. ok is false when the operator backed out.
func (d *Deps) resolveApply(ctx context.Context, o Options) (t target, ok bool, err error) {
	if isDir(d.Chart) {
		return t, false, fmt.Errorf("--chart '%s' is a directory — use --chart-dir", d.Chart)
	}
	switch {
	case o.ChartDir != "":
		return d.localTarget(o.ChartDir, true)
	case o.Version != "":
		return target{version: o.Version}, true, validVersion(o.Version)
	case d.production():
		v, err := d.pinned()
		if err != nil {
			return t, false, err
		}
		return target{version: v}, true, validVersion(v)
	}
	tags, err := d.GitHub.ReleaseTags(ctx, GitHubRepo, 10)
	if err != nil {
		return t, false, err
	}
	// zero releases is fine: the chooser then offers just custom/Back
	if !d.Prompt.Interactive() {
		return t, false, fmt.Errorf("no terminal to choose on: %w (pass --version or --chart-dir)", ui.ErrNoTerminal)
	}
	choice, err := d.Prompt.Choose("platform-crds version", append(tags, "custom", ui.Back)...)
	if err != nil {
		return t, false, err
	}
	switch choice {
	case ui.Back:
		return t, false, nil
	case "custom":
		if !d.Prompt.Interactive() {
			return t, false, fmt.Errorf("no terminal to ask for the checkout on: %w (pass --chart-dir)", ui.ErrNoTerminal)
		}
		dir, err := d.Prompt.Path("Local platform-crds checkout — rendered by hack/render.sh (Tab completes, empty = back):", d.PathPrefill)
		if err != nil || dir == "" {
			return t, false, err
		}
		return d.localTarget(dir, true)
	}
	return target{version: choice}, true, validVersion(choice)
}

func (d *Deps) localTarget(dir string, strict bool) (target, bool, error) {
	abs, version, files, err := checkDir(dir, strict)
	if err != nil {
		return target{}, false, err
	}
	src := abs
	if g := d.GitInfo; g != nil {
		if info := g(abs); info != "" {
			src += " (" + info + ")"
		}
	}
	d.P.Highlight("platform-crds %s from %s — %d CRD files", version, src, files)
	return target{dir: abs}, true, nil
}

// Apply is the crds menu item: diff, confirm, re-read, strip ArgoCD tracking,
// wait out terminating CRDs, server-side apply (always, also when the diff is
// empty, so field ownership lands on our manager), wait Established, assert.
// Declining is not an error.
func Apply(ctx context.Context, d *Deps, o Options) error {
	t, ok, err := d.resolveApply(ctx, o)
	if err != nil || !ok {
		return err
	}
	v := t.label()
	b, err := d.fetch(ctx, v, t.dir)
	if err != nil {
		return err
	}
	live, err := d.preflight(ctx, b) // on the pre-diff snapshot on purpose: it gates the diff the operator is shown
	if err != nil {
		return err
	}
	df, err := d.diff(ctx, b, live)
	if err != nil {
		return fmt.Errorf("diff failed: %w", err)
	}
	d.plan(v, b, live, df, true)
	if df.drift() {
		if d.Paged {
			if err := d.Prompt.Pager(df.Text); err != nil {
				return err
			}
		} else {
			fmt.Fprint(d.Out, df.Text)
		}
		ok, err := d.confirm("Apply platform-crds " + v + "?")
		if err != nil {
			return err
		}
		if !ok {
			d.P.Info("Skipped platform-crds %s", v)
			return nil
		}
	} else {
		// Nothing differs, but a bundle CRD that is mid-deletion is NOT "in sync": it still has
		// its spec so it diffs identically, and it is about to disappear. Never report success
		// there; fail, so a declined or non-interactive run cannot leave the type on its way out.
		dying := terminating(live)
		ok := d.Yes
		if !ok && d.Prompt.Interactive() {
			if ok, err = d.Prompt.Confirm(reapplyPrompt(v, live, dying)); err != nil {
				return err
			}
		}
		if !ok {
			if len(dying) > 0 {
				d.P.Error("not applied, and %d CRD(s) are still terminating: %s", len(dying), join(dying))
				d.P.Plain("   the type disappears when deletion completes — re-run once it clears, or answer yes to wait and recreate it")
				return errors.New("terminating CRDs not applied")
			}
			d.P.Info("✅ platform-crds %s — %s in sync, nothing applied", v, strings.Replace(b.scope(), " — profiles not enabled on this cluster are excluded", "", 1))
			return nil
		}
	}
	// What an apply leaves is deliberate: it creates what is missing and moves field ownership
	// onto our manager, but removes nothing a previous installer left on these CRDs (chart
	// labels, a spec.conversion webhook an older cert-manager chart wrote). Server-side apply
	// cannot remove a field it does not set, and `replace` erases finalizers. The leftovers are
	// inert today: every CRD in the bundle serves one version. REVISIT the first time the bundle
	// serves two versions of a type, because a webhook pointing at a dead service then breaks reads.
	//
	// The snapshot predates the diff, the pager and the confirm — an unbounded human gap. Re-read
	// it so the terminating check sees the cluster as it is now.
	pre, err := readLive(ctx, d.Cluster, b)
	if err != nil {
		return fmt.Errorf("cannot re-read CRDs after the confirm — nothing was applied: %w", err)
	}
	c, err := d.clear(ctx, pre)
	if err != nil {
		return err
	}
	if err := d.doApply(ctx, b, c); err != nil {
		return fmt.Errorf("CRD apply failed: %w", err)
	}
	if err := d.Cluster.WaitCRDsEstablished(ctx, sortedCopy(b.names), d.wait()); err != nil {
		return fmt.Errorf("some CRDs did not become Established: %w", err)
	}
	post, err := readLive(ctx, d.Cluster, b)
	if err != nil {
		return err
	}
	if len(post) != b.total() {
		return fmt.Errorf("expected %d bundle CRDs live after apply, found %d", b.total(), len(post))
	}
	// Established stays True on a CRD that is being deleted, so the wait above cannot see this
	if dying := terminating(post); len(dying) > 0 {
		return fmt.Errorf("the bundle WAS applied, but still terminating (the type is unusable until it clears): %s", join(dying))
	}
	d.P.Info("✅ %d CRDs Established at platform-crds %s (applied as %s)", b.total(), v, FieldManager)
	return nil
}

// reapplyPrompt says why a re-apply is worth it, from data already on hand.
func reapplyPrompt(v string, live []*unstructured.Unstructured, dying []string) string {
	if len(dying) > 0 {
		return fmt.Sprintf("%d CRD(s) are terminating and will disappear. Apply to wait for them and recreate?", len(dying))
	}
	n := len(argoTrackedAnnotation(live)) + len(argoTrackedLabel(live))
	if n > 0 {
		return fmt.Sprintf("No content changes, but %d CRD(s) carry ArgoCD tracking metadata. Apply to strip it?", n)
	}
	return fmt.Sprintf("Nothing to apply. Re-apply platform-crds %s anyway to refresh field ownership?", v)
}

// ErrDrift is Check's verdict: the cluster does not match the bundle.
var ErrDrift = errors.New("drift")

// Check is the apply's read-only preflight, stopped before every mutation: it
// never reaches clear or doApply, and the diff is a server-side dry run. It
// returns ErrDrift for drift and any other error for a failure.
func Check(ctx context.Context, d *Deps, o Options) error {
	if isDir(d.Chart) {
		return fmt.Errorf("--chart '%s' is a directory — check reads a released bundle, use --chart-dir", d.Chart)
	}
	var t target
	switch {
	case o.ChartDir != "":
		var err error
		if t, _, err = d.localTarget(o.ChartDir, false); err != nil {
			return err
		}
	case o.Version != "":
		t = target{version: o.Version}
	default:
		// Read directly: a dev chooser belongs to apply, not to a non-interactive check.
		v, err := d.pinned()
		if err != nil {
			return err
		}
		t = target{version: v}
	}
	if t.dir == "" {
		if err := validVersion(t.version); err != nil {
			return err
		}
	}
	v := t.label()
	b, err := d.fetch(ctx, v, t.dir)
	if err != nil {
		return err
	}
	live, err := d.preflight(ctx, b)
	if err != nil {
		return err
	}
	df, err := d.diff(ctx, b, live)
	if err != nil {
		return fmt.Errorf("diff failed: %w", err)
	}
	d.plan(v, b, live, df, false)
	// a CRD that does not exist yet is not owned either
	un, missing := unowned(live, FieldManager), b.total()-len(live)
	if n := len(un) + missing; n > 0 {
		detail := join(un)
		if missing > 0 {
			detail = strings.TrimSpace(fmt.Sprintf("%s (%d not on the cluster yet)", detail, missing))
		}
		fmt.Fprintf(d.Out, "  own:    %d not yet owned by %s: %s\n", n, FieldManager, detail)
	} else {
		fmt.Fprintf(d.Out, "  own:    all %d owned by %s\n", b.total(), FieldManager)
	}
	// A CRD that is being deleted still has its spec, so it diffs as identical, but it is not
	// in sync: the type is about to disappear. apply refuses to call that state in sync too.
	if dying := terminating(live); len(dying) > 0 {
		d.P.Warn("drift: %d CRD(s) are terminating and will disappear: %s", len(dying), join(dying))
		return ErrDrift
	}
	if bad := notEstablished(live); len(bad) > 0 {
		d.P.Warn("drift: %d CRD(s) are not Established (check their status.conditions, e.g. another CRD owning the same kind): %s", len(bad), join(bad))
		return ErrDrift
	}
	if df.drift() {
		d.P.Warn("drift: this cluster does not match platform-crds %s", v)
		return ErrDrift
	}
	d.P.Info("✅ in sync with platform-crds %s", v)
	return nil
}

// secs prints a duration the way the bash command did: 120s, not 2m0s.
func secs(d time.Duration) string { return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds())) }
