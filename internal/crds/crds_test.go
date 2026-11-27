package crds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/kube"
	"github.com/glueops/captain-utils/internal/ui"
)

func crdDoc(name string, versions ...string) string {
	if len(versions) == 0 {
		versions = []string{"v1"}
	}
	s := "---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + name + "\nspec:\n  versions:\n"
	for _, v := range versions {
		s += "    - name: " + v + "\n      served: true\n"
	}
	return s
}

type fakeSource struct {
	manifest string
	union    int
	calls    []string
	err      error
}

func (f *fakeSource) CRDBundle(_ context.Context, ref, version, values string) (*helmx.CRDBundle, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s@%s values=%v", ref, version, values != ""))
	if f.err != nil {
		return nil, f.err
	}
	return &helmx.CRDBundle{Manifest: []byte(f.manifest), Union: f.union}, nil
}

// fakeCluster is an in-memory CRD store; mutations are recorded in order.
type fakeCluster struct {
	live   map[string]*unstructured.Unstructured
	calls  []string
	order  []string // migrate and apply, in order
	delErr error
	// failures to inject
	applyFail  map[string]error
	estErr     error
	afterApply func(f *fakeCluster, name string)
	// afterList runs after each ListCRDs (to change the cluster between reads)
	afterList func(f *fakeCluster, n int)
	lists     int
}

func newCluster() *fakeCluster { return &fakeCluster{live: map[string]*unstructured.Unstructured{}} }

func (f *fakeCluster) put(doc string, mut func(*unstructured.Unstructured)) {
	objs, err := kube.Objects([]byte(doc))
	if err != nil {
		panic(err)
	}
	for _, o := range objs {
		if mut != nil {
			mut(o)
		}
		f.live[o.GetName()] = o
	}
}

func (f *fakeCluster) ListCRDs(context.Context) ([]*unstructured.Unstructured, error) {
	f.lists++
	var out []*unstructured.Unstructured
	for _, o := range f.live {
		out = append(out, o.DeepCopy())
	}
	if f.afterList != nil {
		defer f.afterList(f, f.lists)
	}
	return out, nil
}

func (f *fakeCluster) Apply(_ context.Context, obj *unstructured.Unstructured, o kube.ApplyOptions) (*unstructured.Unstructured, error) {
	if o.FieldManager != FieldManager || !o.Force {
		return nil, fmt.Errorf("apply must use manager %s with force", FieldManager)
	}
	m := obj.DeepCopy()
	if cur := f.live[obj.GetName()]; cur != nil {
		// what others own survives an apply: deletion state, and labels/annotations we do not set
		m.SetDeletionTimestamp(cur.GetDeletionTimestamp())
		l := cur.GetLabels()
		for k, v := range m.GetLabels() {
			if l == nil {
				l = map[string]string{}
			}
			l[k] = v
		}
		m.SetLabels(l)
		a := cur.GetAnnotations()
		for k, v := range m.GetAnnotations() {
			if a == nil {
				a = map[string]string{}
			}
			a[k] = v
		}
		m.SetAnnotations(a)
	}
	if o.DryRun {
		return m, nil
	}
	if err := f.applyFail[obj.GetName()]; err != nil {
		return nil, err
	}
	f.calls = append(f.calls, "apply "+obj.GetName())
	f.order = append(f.order, "apply "+obj.GetName())
	m.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: FieldManager, Operation: "Apply"}})
	f.live[obj.GetName()] = m
	if f.afterApply != nil {
		f.afterApply(f, obj.GetName())
	}
	return m, nil
}

func (f *fakeCluster) RemoveCRDAnnotation(_ context.Context, n, k string) error {
	f.calls = append(f.calls, "strip annotation "+k+" "+n)
	a := f.live[n].GetAnnotations()
	delete(a, k)
	f.live[n].SetAnnotations(a)
	return nil
}
func (f *fakeCluster) RemoveCRDLabel(_ context.Context, n, k string) error {
	f.calls = append(f.calls, "strip label "+k+" "+n)
	l := f.live[n].GetLabels()
	delete(l, k)
	f.live[n].SetLabels(l)
	return nil
}
func (f *fakeCluster) MigrateCRDToSSA(_ context.Context, n, m string) (bool, error) {
	f.order = append(f.order, "migrate "+n)
	return false, nil
}
func (f *fakeCluster) WaitCRDsDeleted(_ context.Context, names []string, _ time.Duration) error {
	f.calls = append(f.calls, "wait deleted "+strings.Join(names, ","))
	if f.delErr != nil {
		return f.delErr
	}
	for _, n := range names {
		delete(f.live, n)
	}
	return nil
}
func (f *fakeCluster) WaitCRDsEstablished(_ context.Context, names []string, _ time.Duration) error {
	f.calls = append(f.calls, "wait established "+strings.Join(names, ","))
	return f.estErr
}

// drifted makes the live CRD differ from the bundle's.
func (f *fakeCluster) drifted(name string) {
	_ = unstructured.SetNestedSlice(f.live[name].Object, []interface{}{map[string]interface{}{"name": "v0", "served": true}}, "spec", "versions")
}

func (f *fakeCluster) mutations() []string { return f.calls }

// noTTY is a prompter for a run without a terminal.
type noTTY struct{ ui.Script }

func (noTTY) Interactive() bool { return false }

type harness struct {
	d      *Deps
	src    *fakeSource
	cl     *fakeCluster
	prompt ui.Prompter
	script *ui.Script
	out    *bytes.Buffer
	errs   *bytes.Buffer
}

const pins = "versions:\n  - name: platform_crds_version\n    version: v0.1.6\n"

func newHarness(t *testing.T, files map[string]string, tty bool, answers ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	if files == nil {
		files = map[string]string{"VERSIONS/glueops.yaml": pins, "platform.yaml": "a: 1\n"}
	}
	for n, body := range files {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{src: &fakeSource{manifest: crdDoc("a.x.io") + crdDoc("b.x.io"), union: 2}, cl: newCluster(), out: &bytes.Buffer{}, errs: &bytes.Buffer{}}
	h.script = &ui.Script{Answers: answers}
	if tty {
		h.prompt = h.script
	} else {
		h.prompt = &noTTY{Script: *h.script}
	}
	h.d = &Deps{Source: h.src, Cluster: h.cl, Prompt: h.prompt,
		P: ui.New(h.out, h.errs, true), Out: h.out, Repo: captain.Repo{Dir: dir}, Wait: time.Second,
		GitInfo: func(string) string { return "main@abc123" }}
	return h
}

func (h *harness) syncLive() {
	h.cl.put(h.src.manifest, func(o *unstructured.Unstructured) {
		o.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: FieldManager, Operation: "Apply"}})
	})
}

func TestApplyAppliesAfterConfirm(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	want := "apply a.x.io|apply b.x.io|wait established a.x.io,b.x.io"
	if got := strings.Join(h.cl.calls, "|"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if !strings.Contains(h.out.String(), "2 new, 0 changed") || !strings.Contains(h.errs.String(), "2 CRDs Established at platform-crds v0.1.6") {
		t.Fatalf("out %q err %q", h.out, h.errs)
	}
	if h.src.calls[0] != "oci://ghcr.repo.gpkg.io/glueops/platform-crds@0.1.6 values=true" {
		t.Fatalf("fetch: %v", h.src.calls)
	}
}

func TestApplyDeclinedChangesNothing(t *testing.T) {
	h := newHarness(t, nil, true, "no")
	res, err := ApplyResult(context.Background(), h.d, Options{Version: "v0.1.6"})
	if !errors.Is(err, ui.ErrDeclined) {
		t.Fatalf("a declined confirm must be ui.ErrDeclined (exit 3), got %v", err)
	}
	if len(h.cl.calls) != 0 || !strings.Contains(h.errs.String(), "Skipped platform-crds v0.1.6") {
		t.Fatalf("%v %q", h.cl.calls, h.errs)
	}
	if res == nil || len(res.Applied) != 0 || len(res.Unchanged) != 0 || len(res.Failed) != 0 || res.Established {
		t.Fatalf("a declined run applied nothing: %+v", res)
	}
}

func TestApplyWithoutTerminalNeedsYes(t *testing.T) {
	h := newHarness(t, nil, false)
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if !errors.Is(err, ui.ErrNoTerminal) || len(h.cl.calls) != 0 {
		t.Fatalf("%v %v", err, h.cl.calls)
	}
	h = newHarness(t, nil, false)
	h.d.Yes = true
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil || len(h.cl.calls) != 3 {
		t.Fatalf("%v %v", err, h.cl.calls)
	}
}

func TestApplyInSyncWithoutTerminalIsQuietSuccess(t *testing.T) {
	h := newHarness(t, nil, false)
	h.syncLive()
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	if len(h.cl.calls) != 0 || !strings.Contains(h.errs.String(), "2 CRDs in sync, nothing applied") {
		t.Fatalf("%v %q", h.cl.calls, h.errs)
	}
}

func TestApplyInSyncOffersReapplyAndStripsArgoTracking(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.cl.put(h.src.manifest, func(o *unstructured.Unstructured) {
		if o.GetName() == "a.x.io" {
			o.SetAnnotations(map[string]string{argoTrackingID: "x"})
			o.SetLabels(map[string]string{argoInstance: "y"})
		}
	})
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	if got := h.script.Asked; len(got) != 0 {
		t.Log(got)
	}
	want := "strip annotation argocd.argoproj.io/tracking-id a.x.io|strip label argocd.argoproj.io/instance a.x.io|apply a.x.io|apply b.x.io|wait established a.x.io,b.x.io"
	if got := strings.Join(h.cl.calls, "|"); got != want {
		t.Fatalf("got %s", got)
	}
	if !strings.Contains(h.out.String(), "strip:  ArgoCD tracking-id from 1, instance label from 1") {
		t.Fatalf("plan: %q", h.out)
	}
}

func TestApplyTerminatingNotInSyncWhenDeclinedFails(t *testing.T) {
	// the Gate CRD case: a clean diff, but the type is about to disappear
	h := newHarness(t, nil, false)
	h.cl.put(h.src.manifest, func(o *unstructured.Unstructured) {
		if o.GetName() == "b.x.io" {
			now := metav1.NewTime(time.Now())
			o.SetDeletionTimestamp(&now)
		}
	})
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || len(h.cl.calls) != 0 || !strings.Contains(h.errs.String(), "still terminating: b.x.io") {
		t.Fatalf("%v %v %q", err, h.cl.calls, h.errs)
	}
}

func TestApplyWaitsOutTerminatingBeforeApplying(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.syncLive()
	now := metav1.NewTime(time.Now())
	h.cl.live["b.x.io"].SetDeletionTimestamp(&now)
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	want := "wait deleted b.x.io|apply a.x.io|apply b.x.io|wait established a.x.io,b.x.io"
	if got := strings.Join(h.cl.calls, "|"); got != want {
		t.Fatalf("got %s", got)
	}
}

func TestApplyTerminatingStillStuckAppliesNothing(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.syncLive()
	now := metav1.NewTime(time.Now())
	h.cl.live["b.x.io"].SetDeletionTimestamp(&now)
	h.cl.delErr = &kube.TimeoutError{Timeout: time.Second, Pending: []string{"b.x.io"}}
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || err.Error() != "still terminating after 1s — the bundle was NOT applied: b.x.io" {
		t.Fatalf("%v", err)
	}
	for _, c := range h.cl.calls {
		if strings.HasPrefix(c, "apply ") {
			t.Fatalf("applied despite a stuck CRD: %v", h.cl.calls)
		}
	}
}

func TestApplyRereadsLiveAfterConfirm(t *testing.T) {
	// a CRD that starts terminating while the operator reads the diff is still waited out
	h := newHarness(t, nil, true, "yes")
	h.syncLive()
	h.cl.drifted("a.x.io") // a diff, so the confirm happens
	h.cl.afterList = func(f *fakeCluster, n int) {
		if n == 1 {
			now := metav1.NewTime(time.Now())
			f.live["b.x.io"].SetDeletionTimestamp(&now)
		}
	}
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	if h.cl.calls[0] != "wait deleted b.x.io" {
		t.Fatalf("got %v", h.cl.calls)
	}
}

func TestApplyRefusesStorageVersionRemoval(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.cl.put(crdDoc("a.x.io", "v1beta1"), func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedStringSlice(o.Object, []string{"v1beta1"}, "status", "storedVersions")
	})
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || len(h.cl.calls) != 0 || !strings.Contains(h.errs.String(), "a.x.io stores v1beta1, which platform-crds no longer defines") {
		t.Fatalf("%v %v %q", err, h.cl.calls, h.errs)
	}
}

func TestStoredVersionNoLongerServedWarns(t *testing.T) {
	b := &bundle{}
	objs, _ := kube.Objects([]byte("---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata: {name: a.x.io}\nspec:\n  versions:\n    - {name: v1, served: true}\n    - {name: v1beta1, served: false}\n"))
	b.objs = objs
	live := objs[0].DeepCopy()
	_ = unstructured.SetNestedStringSlice(live.Object, []string{"v1beta1"}, "status", "storedVersions")
	errs, warns := storedVersions(b, []*unstructured.Unstructured{live})
	if len(errs) != 0 || len(warns) != 1 || !strings.Contains(warns[0], "no longer served") {
		t.Fatalf("%v %v", errs, warns)
	}
}

func TestBundleRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ manifest, want string }{
		"should refuse a bundle with no CRDs":                                         {"---\n", "rendered no CRDs"},
		"should refuse non-CRD objects":                                               {crdDoc("a.x.io") + "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n", "non-CRD objects, refusing to apply: ConfigMap/cm"},
		"should refuse a CRD declared twice (live counts are by unique name)":         {crdDoc("a.x.io") + crdDoc("b.x.io") + crdDoc("a.x.io"), "declares a CRD more than once, refusing to apply: a.x.io"},
		"should refuse a CRD file that merged into the previous one (no leading ---)": {"---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: a.x.io\nspec: {}\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: b.x.io\nspec: {}\n", "does not parse as 2 separate CRD documents"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, true, "yes")
			h.src.manifest = tc.manifest
			err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(h.cl.calls) != 0 {
				t.Fatalf("%v %v", err, h.cl.calls)
			}
		})
	}
}

func TestProductionNeedsPlatformYaml(t *testing.T) {
	h := newHarness(t, map[string]string{"VERSIONS/glueops.yaml": pins}, true)
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || !strings.Contains(err.Error(), "no platform.yaml") {
		t.Fatalf("%v", err)
	}
}

func TestScopeSaysWhenProfilesExcludeCRDs(t *testing.T) {
	h := newHarness(t, nil, true, "no")
	h.src.union = 5
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); !errors.Is(err, ui.ErrDeclined) {
		t.Fatal(err)
	}
	if !strings.Contains(h.errs.String(), "2 of 5 CRDs — profiles not enabled on this cluster are excluded") {
		t.Fatalf("%q", h.errs)
	}
}

func TestVersionMustBeAReleaseTag(t *testing.T) {
	for _, v := range []string{"main", "v1.2", "1.2.3-rc1"} {
		h := newHarness(t, nil, true)
		err := Apply(context.Background(), h.d, Options{Version: v})
		if err == nil || !strings.Contains(err.Error(), "must be a release tag vX.Y.Z") || len(h.src.calls) != 0 {
			t.Fatalf("%s: %v", v, err)
		}
	}
}

func TestDuplicatedPinIsRejected(t *testing.T) {
	dup := pins + "  - name: platform_crds_version\n    version: v0.1.7\n"
	h := newHarness(t, map[string]string{"VERSIONS/glueops.yaml": dup, "platform.yaml": "a: 1\n"}, true)
	if err := Apply(context.Background(), h.d, Options{}); err == nil || !strings.Contains(err.Error(), "want exactly one") {
		t.Fatalf("%v", err)
	}
}

func TestChooserOffersPinCustomAndBack(t *testing.T) {
	h := newHarness(t, nil, true, "Back")
	if err := Apply(context.Background(), h.d, Options{}); err != nil || len(h.src.calls) != 0 {
		t.Fatalf("%v %v", err, h.src.calls)
	}
	if got := h.script.Asked[0]; got != "CHOOSE platform-crds version [v0.1.6|custom|Back]" {
		t.Fatal(got)
	}
}

func TestChooserAppliesTheChosenPin(t *testing.T) {
	h := newHarness(t, nil, true, "v0.1.6", "yes")
	if err := Apply(context.Background(), h.d, Options{}); err != nil || len(h.src.calls) != 1 {
		t.Fatalf("%v %v", err, h.src.calls)
	}
}

func TestChooserRefusesABranchLikePin(t *testing.T) {
	branch := "versions:\n  - name: platform_crds_version\n    version: main\n"
	h := newHarness(t, map[string]string{"VERSIONS/glueops.yaml": branch, "platform.yaml": "a: 1\n"}, true)
	if err := Apply(context.Background(), h.d, Options{}); err == nil || !strings.Contains(err.Error(), "must be a release tag vX.Y.Z") || len(h.src.calls) != 0 {
		t.Fatalf("%v", err)
	}
}

func TestWithoutTerminalApplyAndCheckNeedAnExplicitVersion(t *testing.T) {
	for name, run := range map[string]func(*harness) error{
		"apply": func(h *harness) error { return Apply(context.Background(), h.d, Options{}) },
		"check": func(h *harness) error { return Check(context.Background(), h.d, Options{}) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, false)
			err := run(h)
			if !errors.Is(err, ui.ErrUsage) || !strings.Contains(err.Error(), "pass --version or --chart-dir") {
				t.Fatalf("%v", err)
			}
			if len(h.src.calls) != 0 || len(h.cl.calls) != 0 {
				t.Fatalf("fetched or mutated: %v %v", h.src.calls, h.cl.calls)
			}
		})
	}
	// and with --version they run
	h := newHarness(t, nil, false)
	h.syncLive()
	if err := Check(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
}

func localCheckout(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	must := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("Chart.yaml", "name: "+name+"\nversion: 0.2.0\n")
	must("crds/a.yaml", crdDoc("a.x.io"))
	return dir
}

func TestApplyFromLocalCheckout(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	dir := localCheckout(t, "platform-crds")
	if err := Apply(context.Background(), h.d, Options{ChartDir: dir}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.src.calls[0], dir+"@ values=true") || !strings.Contains(h.errs.String(), "platform-crds 0.2.0 from "+dir+" (main@abc123) — 2 CRD files") && !strings.Contains(h.errs.String(), "CRD files") {
		t.Fatalf("%v %q", h.src.calls, h.errs)
	}
	if !strings.Contains(h.errs.String(), "Established at platform-crds local") {
		t.Fatalf("%q", h.errs)
	}
}

func TestLocalCheckoutRefusals(t *testing.T) {
	notSep := localCheckout(t, "platform-crds")
	_ = os.WriteFile(filepath.Join(notSep, "crds", "b.yaml"), []byte("apiVersion: v1\n"), 0o644)
	for name, tc := range map[string]struct{ dir, want string }{
		"should refuse a missing directory":         {filepath.Join(t.TempDir(), "nope"), "is not a directory"},
		"should refuse another chart":               {localCheckout(t, "glueops-platform"), "is chart 'glueops-platform', not platform-crds"},
		"should refuse a crds file without leading": {notSep, "must start with ---"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, true, "yes")
			err := Apply(context.Background(), h.d, Options{ChartDir: tc.dir})
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(h.cl.calls) != 0 {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestChartOverrideThatIsADirectoryIsRefused(t *testing.T) {
	h := newHarness(t, nil, true)
	h.d.Chart = t.TempDir()
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err == nil || !strings.Contains(err.Error(), "use --chart-dir") {
		t.Fatalf("%v", err)
	}
	if err := Check(context.Background(), h.d, Options{Version: "v0.1.6"}); err == nil || !strings.Contains(err.Error(), "use --chart-dir") {
		t.Fatalf("%v", err)
	}
}

func TestCheckInSyncReadsPinAndMutatesNothing(t *testing.T) {
	h := newHarness(t, nil, false)
	h.syncLive()
	if err := Check(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	if len(h.cl.mutations()) != 0 || !strings.Contains(h.errs.String(), "in sync with platform-crds v0.1.6") || !strings.Contains(h.out.String(), "own:    all 2 owned by glueops-platform-crds") {
		t.Fatalf("%v %q %q", h.cl.calls, h.errs, h.out)
	}
}

// -o json's inSync is true exactly when the check passes: deleting that assignment must fail a test.
func TestCheckResultInSyncIsTrueWhenNothingDrifts(t *testing.T) {
	h := newHarness(t, nil, false)
	h.syncLive()
	rep, err := CheckResult(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err != nil || rep == nil || !rep.InSync || len(rep.Reasons) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	h = newHarness(t, nil, false) // nothing on the cluster: drift, not in sync
	if rep, err = CheckResult(context.Background(), h.d, Options{Version: "v0.1.6"}); !errors.Is(err, ErrDrift) || rep == nil || rep.InSync {
		t.Fatalf("%v %+v", err, rep)
	}
}

func TestCheckDriftExitsWithErrDriftAndMutatesNothing(t *testing.T) {
	h := newHarness(t, nil, false)
	h.cl.put(h.src.manifest, func(o *unstructured.Unstructured) {
		if o.GetName() == "a.x.io" {
			o.SetLabels(map[string]string{argoInstance: "y"}) // would be stripped by apply, not by check
		}
	})
	h.cl.drifted("a.x.io")
	h.cl.drifted("b.x.io")
	err := Check(context.Background(), h.d, Options{Version: "v0.1.6"})
	if !errors.Is(err, ErrDrift) || len(h.cl.mutations()) != 0 {
		t.Fatalf("%v %v", err, h.cl.calls)
	}
	if !strings.Contains(h.out.String(), "0 new, 2 changed") || !strings.Contains(h.out.String(), "own:    2 not yet owned by glueops-platform-crds: a.x.io b.x.io") {
		t.Fatalf("%q", h.out)
	}
}

func TestCheckAtATerminalFailsWithoutAPin(t *testing.T) {
	h := newHarness(t, map[string]string{"platform.yaml": "a: 1\n"}, true)
	err := Check(context.Background(), h.d, Options{})
	if err == nil || errors.Is(err, ErrDrift) || errors.Is(err, ui.ErrUsage) {
		t.Fatalf("%v", err)
	}
}

func TestCheckReportsCRDMissingFromClusterAsNew(t *testing.T) {
	h := newHarness(t, nil, false)
	err := Check(context.Background(), h.d, Options{Version: "v0.1.6"})
	if !errors.Is(err, ErrDrift) || !strings.Contains(h.out.String(), "2 new, 0 changed") || !strings.Contains(h.out.String(), "own:    2 not yet owned by glueops-platform-crds: (2 not on the cluster yet)") {
		t.Fatalf("%v %q", err, h.out)
	}
}

func TestDiffShowsTheChange(t *testing.T) {
	h := newHarness(t, nil, true, "no")
	h.syncLive()
	h.cl.drifted("a.x.io")
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); !errors.Is(err, ui.ErrDeclined) {
		t.Fatal(err)
	}
	out := h.out.String()
	if !strings.Contains(out, "0 new, 1 changed") || !strings.Contains(out, "diff -u -N LIVE/apiextensions.k8s.io.v1.CustomResourceDefinition..a.x.io") || !strings.Contains(out, "-        - name: v0") {
		t.Fatalf("%q", out)
	}
}

// The guards the bash contract tests (structural contract.sh) enforced.
func TestContractOneMutatingApplyAndNeverReplace(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var applies []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				ast.Inspect(fd, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.SelectorExpr:
						switch x.Sel.Name {
						case "Replace", "Update", "Create", "Delete", "Patch":
							if id, ok := x.X.(*ast.Ident); ok && id.Name == "strings" {
								return true
							}
							t.Errorf("%s uses %s: CRDs are only server-side applied", fd.Name.Name, x.Sel.Name)
						}
					case *ast.CompositeLit:
						if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ApplyOptions" {
							dry := false
							for _, e := range x.Elts {
								if kv, ok := e.(*ast.KeyValueExpr); ok && fmt.Sprint(kv.Key) == "DryRun" {
									dry = true
								}
							}
							if !dry {
								applies = append(applies, fd.Name.Name)
							}
						}
					}
					return true
				})
			}
		}
	}
	if len(applies) != 1 || applies[0] != "doApply" {
		t.Fatalf("mutating Apply calls live in %v, want exactly doApply", applies)
	}
}

func TestDoApplyRefusesWithoutClearance(t *testing.T) {
	h := newHarness(t, nil, true)
	b, err := parseBundle("v", &helmx.CRDBundle{Manifest: []byte(h.src.manifest)})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.d.doApply(context.Background(), b, clearance{}); err == nil || len(h.cl.calls) != 0 {
		t.Fatalf("%v %v", err, h.cl.calls)
	}
}

func TestApplyMigratesClientSideApplyOwnershipBeforeEachApply(t *testing.T) {
	// kubectl apply --server-side does this first; without it the last-applied annotation
	// and the kubectl-client-side-apply manager stay on the CRD for good
	h := newHarness(t, nil, true, "yes")
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	want := "migrate a.x.io|apply a.x.io|migrate b.x.io|apply b.x.io"
	if got := strings.Join(h.cl.order, "|"); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestCheckTerminatingCRDIsDriftNotInSync(t *testing.T) {
	// the diff of a CRD that is being deleted is clean, which is exactly why a clean diff cannot be the verdict
	h := newHarness(t, nil, false)
	h.syncLive()
	now := metav1.NewTime(time.Now())
	h.cl.live["b.x.io"].SetDeletionTimestamp(&now)
	err := Check(context.Background(), h.d, Options{Version: "v0.1.6"})
	if !errors.Is(err, ErrDrift) || len(h.cl.mutations()) != 0 || !strings.Contains(h.errs.String(), "drift: 1 CRD(s) are terminating and will disappear: b.x.io") || strings.Contains(h.errs.String(), "in sync") {
		t.Fatalf("%v %v %q", err, h.cl.calls, h.errs)
	}
}

func TestWaitIsPrintedInSecondsLikeTheBashCommand(t *testing.T) {
	if got := secs(2 * time.Minute); got != "120s" {
		t.Fatal(got)
	}
}

func TestApplyKeepsGoingAfterOneCRDFailsAndReportsEveryFailure(t *testing.T) {
	// as kubectl apply does: a bad CRD does not stop the others, and no failure is hidden
	h := newHarness(t, nil, true, "yes")
	h.src.manifest = crdDoc("a.x.io") + crdDoc("b.x.io") + crdDoc("c.x.io")
	h.cl.applyFail = map[string]error{"a.x.io": errors.New("webhook denied"), "c.x.io": errors.New("quota")}
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || !strings.Contains(err.Error(), "apply a.x.io: webhook denied") || !strings.Contains(err.Error(), "apply c.x.io: quota") {
		t.Fatalf("%v", err)
	}
	if got := strings.Join(h.cl.calls, "|"); got != "apply b.x.io" {
		t.Fatalf("b must still be applied, and nothing may be waited on after a failure: %s", got)
	}
}

func TestApplyFailsWhenCRDsDoNotBecomeEstablished(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.cl.estErr = &kube.TimeoutError{Timeout: time.Second, Pending: []string{"b.x.io"}}
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || !strings.Contains(err.Error(), "some CRDs did not become Established") || !strings.Contains(err.Error(), "b.x.io") {
		t.Fatalf("%v", err)
	}
}

func TestApplyAssertsEveryBundleCRDIsLiveAfterwards(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.cl.afterApply = func(f *fakeCluster, name string) {
		if name == "b.x.io" {
			delete(f.live, name) // gone again before the post-apply read
		}
	}
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || !strings.Contains(err.Error(), "expected 2 bundle CRDs live after apply, found 1") {
		t.Fatalf("%v", err)
	}
}

func TestApplyFailsWhenACRDStartsTerminatingDuringTheApply(t *testing.T) {
	// Established stays True on a CRD being deleted, so only the post-apply read can see this
	h := newHarness(t, nil, true, "yes")
	h.cl.afterApply = func(f *fakeCluster, name string) {
		if name == "a.x.io" {
			now := metav1.NewTime(time.Now())
			f.live[name].SetDeletionTimestamp(&now)
		}
	}
	err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || !strings.Contains(err.Error(), "the bundle WAS applied, but still terminating") || !strings.Contains(err.Error(), "a.x.io") {
		t.Fatalf("%v", err)
	}
}

func TestCheckPrintsNoApplyOnlyPlanLines(t *testing.T) {
	// regression: check reused the apply plan and said "strip", "the apply waits" and "after you confirm"
	h := newHarness(t, nil, false)
	h.syncLive()
	if err := Check(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	for _, apply := range []string{"strip:", "wait:", "keep:", "migrate:"} {
		if strings.Contains(h.out.String(), apply) {
			t.Fatalf("check printed %q: %q", apply, h.out)
		}
	}
}

func TestApplyPlanSaysClientSideApplyOwnershipMigrates(t *testing.T) {
	// regression: the diff is empty for a client-side-applied CRD whose spec matches, yet apply drops
	// last-applied-configuration; the plan must say so instead of only "no content changes"
	h := newHarness(t, nil, true, "yes")
	h.cl.put(h.src.manifest, func(o *unstructured.Unstructured) {
		o.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: FieldManager, Operation: "Apply"}})
		if o.GetName() == "a.x.io" {
			o.SetAnnotations(map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"})
		}
	})
	if err := Apply(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "no content changes") || !strings.Contains(h.out.String(), "migrate: client-side-apply ownership of 1 CRD(s)") {
		t.Fatalf("%q", h.out)
	}
}

func TestCheckCRDThatIsNotEstablishedIsDriftNotInSync(t *testing.T) {
	// regression (found on a k3d cluster): another CRD owning the same kind leaves NamesAccepted=False and
	// Established=False; the diff is clean and the CRD is owned, yet apply fails on it, so check must not say "in sync"
	h := newHarness(t, nil, false)
	h.syncLive()
	_ = unstructured.SetNestedSlice(h.cl.live["b.x.io"].Object, []interface{}{
		map[string]interface{}{"type": "Established", "status": "False", "reason": "NotAccepted"},
	}, "status", "conditions")
	err := Check(context.Background(), h.d, Options{Version: "v0.1.6"})
	if !errors.Is(err, ErrDrift) || len(h.cl.mutations()) != 0 || !strings.Contains(h.errs.String(), "drift: 1 CRD(s) are not Established") || !strings.Contains(h.errs.String(), "same kind): b.x.io") || strings.Contains(h.errs.String(), "in sync") {
		t.Fatalf("%v %v %q", err, h.cl.calls, h.errs)
	}
	// a CRD with no Established condition at all (just created) is not flagged
	h = newHarness(t, nil, false)
	h.syncLive()
	if err := Check(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckInSyncButNotOwnedSaysSoAndExitsZeroByDefault(t *testing.T) {
	// content matches, a manager other than ours owns the fields: apply calls this in sync too, so check
	// exits 0, but "✅ in sync" directly under "N not yet owned" read as a contradiction
	h := newHarness(t, nil, false)
	h.cl.put(h.src.manifest, nil) // live, same content, no managedFields
	if err := Check(context.Background(), h.d, Options{Version: "v0.1.6"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.errs.String(), "in sync with platform-crds v0.1.6 (content); 2 CRD(s) not yet owned by glueops-platform-crds — apply moves ownership") {
		t.Fatalf("%q", h.errs)
	}
}

func TestCheckRequireOwnedFailsOnUnownedCRDs(t *testing.T) {
	h := newHarness(t, nil, false)
	h.cl.put(h.src.manifest, nil)
	err := Check(context.Background(), h.d, Options{Version: "v0.1.6", RequireOwned: true})
	if !errors.Is(err, ErrDrift) || len(h.cl.mutations()) != 0 || !strings.Contains(h.errs.String(), "drift: 2 CRD(s) are not owned by glueops-platform-crds") {
		t.Fatalf("%v %q", err, h.errs)
	}
	// owned: passes
	h = newHarness(t, nil, false)
	h.syncLive()
	if err := Check(context.Background(), h.d, Options{Version: "v0.1.6", RequireOwned: true}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyResultNamesWhatWasApplied(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	res, err := ApplyResult(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "v0.1.6" || !res.Established || strings.Join(res.Applied, ",") != "a.x.io,b.x.io" || len(res.Unchanged) != 0 || len(res.Failed) != 0 {
		t.Fatalf("%+v", res)
	}
	if got := res.Summary(); got != "applied=2 unchanged=0 failed=0" {
		t.Fatal(got)
	}
}

func TestApplyResultSplitsAppliedFromUnchanged(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.syncLive()
	h.cl.drifted("a.x.io")
	res, err := ApplyResult(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Applied, ",") != "a.x.io" || strings.Join(res.Unchanged, ",") != "b.x.io" {
		t.Fatalf("%+v", res)
	}
}

func TestPartialApplyFailureNamesTheFailedCRD(t *testing.T) {
	h := newHarness(t, nil, true, "yes")
	h.cl.applyFail = map[string]error{"b.x.io": errors.New("webhook denied")}
	res, err := ApplyResult(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err == nil || errors.Is(err, ui.ErrDeclined) || errors.Is(err, ui.ErrUsage) {
		t.Fatalf("a partial failure is a plain failure (exit 1): %v", err)
	}
	if len(res.Failed) != 1 || res.Failed[0].Name != "b.x.io" || !strings.Contains(res.Failed[0].Error, "webhook denied") ||
		strings.Join(res.Applied, ",") != "a.x.io" || res.Established {
		t.Fatalf("%+v", res)
	}
	var buf bytes.Buffer
	if err := res.WriteJSON(&buf, false); err != nil || !strings.Contains(buf.String(), `"failed": [`) || !strings.Contains(buf.String(), `"name": "b.x.io"`) {
		t.Fatalf("%v %s", err, buf.String())
	}
}

func TestApplyInSyncResultIsAllUnchanged(t *testing.T) {
	h := newHarness(t, nil, false)
	h.syncLive()
	res, err := ApplyResult(context.Background(), h.d, Options{Version: "v0.1.6"})
	if err != nil || strings.Join(res.Unchanged, ",") != "a.x.io,b.x.io" || len(res.Applied) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
}
