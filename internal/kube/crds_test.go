package kube

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func crdObj(name string, established bool, annotations map[string]interface{}) *unstructured.Unstructured {
	st := "False"
	if established {
		st = "True"
	}
	md := map[string]interface{}{"name": name}
	if annotations != nil {
		md["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": md,
		"status": map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Established", "status": st}}},
	}}
}

func crdClient(objs ...runtime.Object) *Client {
	s := runtime.NewScheme()
	return &Client{Dynamic: dynfake.NewSimpleDynamicClientWithCustomListKinds(s,
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, objs...)}
}

func TestListCRDsIsSortedByName(t *testing.T) {
	c := crdClient(crdObj("b.x.io", true, nil), crdObj("a.x.io", true, nil))
	got, err := c.ListCRDs(context.Background())
	if err != nil || len(got) != 2 || got[0].GetName() != "a.x.io" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestRemoveCRDAnnotationLeavesTheRest(t *testing.T) {
	c := crdClient(crdObj("a.x.io", true, map[string]interface{}{"argocd.argoproj.io/tracking-id": "x", "keep": "me"}))
	if err := c.RemoveCRDAnnotation(context.Background(), "a.x.io", "argocd.argoproj.io/tracking-id"); err != nil {
		t.Fatal(err)
	}
	got, _ := c.ListCRDs(context.Background())
	if a := got[0].GetAnnotations(); len(a) != 1 || a["keep"] != "me" {
		t.Fatalf("%v", a)
	}
}

func TestWaitCRDsEstablished(t *testing.T) {
	c := crdClient(crdObj("a.x.io", true, nil), crdObj("b.x.io", false, nil))
	if err := c.WaitCRDsEstablished(context.Background(), []string{"a.x.io"}, time.Second); err != nil {
		t.Fatal(err)
	}
	err := c.WaitCRDsEstablished(context.Background(), []string{"a.x.io", "b.x.io"}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "b.x.io") || strings.Contains(err.Error(), "a.x.io") {
		t.Fatalf("should name only the CRD that is not Established: %v", err)
	}
}

func TestWaitCRDsDeleted(t *testing.T) {
	c := crdClient(crdObj("a.x.io", true, nil))
	if err := c.WaitCRDsDeleted(context.Background(), []string{"gone.x.io"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitCRDsDeleted(context.Background(), []string{"a.x.io"}, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "a.x.io") {
		t.Fatalf("%v", err)
	}
}

func TestMigrateCRDToSSAHandsClientSideApplyOwnershipToOurManager(t *testing.T) {
	obj := crdObj("a.x.io", true, map[string]interface{}{lastApplied: "{}"})
	obj.SetManagedFields([]metav1.ManagedFieldsEntry{{
		Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: "apiextensions.k8s.io/v1",
		FieldsType: "FieldsV1",
		FieldsV1:   &metav1.FieldsV1{Raw: []byte(`{"f:metadata":{"f:annotations":{"f:kubectl.kubernetes.io/last-applied-configuration":{}}}}`)},
	}})
	c := crdClient(obj)
	migrated, err := c.MigrateCRDToSSA(context.Background(), "a.x.io", "glueops-platform-crds")
	if err != nil || !migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
	got, _ := c.ListCRDs(context.Background())
	mf := got[0].GetManagedFields()
	if len(mf) != 1 || mf[0].Manager != "glueops-platform-crds" || mf[0].Operation != metav1.ManagedFieldsOperationApply {
		t.Fatalf("%+v", mf)
	}
	// nothing left to migrate: a second call is a no-op
	if migrated, err := c.MigrateCRDToSSA(context.Background(), "a.x.io", "glueops-platform-crds"); err != nil || migrated {
		t.Fatalf("second call: migrated=%v err=%v", migrated, err)
	}
}

func TestMigrateCRDToSSALeavesACRDThatClientSideApplyNeverTouched(t *testing.T) {
	c := crdClient(crdObj("a.x.io", true, nil))
	if migrated, err := c.MigrateCRDToSSA(context.Background(), "a.x.io", "glueops-platform-crds"); err != nil || migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
}

func TestMigrateCRDToSSAOfACRDThatIsNotInstalledYetIsNotAnError(t *testing.T) {
	// regression: a fresh cluster has none of the bundle's CRDs, and apply migrates each one first
	c := crdClient()
	if migrated, err := c.MigrateCRDToSSA(context.Background(), "absent.x.io", "glueops-platform-crds"); err != nil || migrated {
		t.Fatalf("migrated=%v err=%v", migrated, err)
	}
}

func TestWaitThatRunsOutInsideTheRateLimiterIsStillATimeout(t *testing.T) {
	// regression: a wait whose deadline passed while client-go's rate limiter was holding the request
	// ("rate: Wait(n=1) would exceed context deadline") surfaced as a generic error, so the operator
	// never saw "still terminating … a finalizer is holding the type open"
	fake := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crdObj("a.x.io", true, nil))
	fake.PrependReactor("get", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		time.Sleep(80 * time.Millisecond)
		return true, nil, errors.New("client rate limiter Wait returned an error: rate: Wait(n=1) would exceed context deadline")
	})
	c := &Client{Dynamic: fake}
	err := c.WaitCRDsDeleted(context.Background(), []string{"a.x.io"}, 40*time.Millisecond)
	var to *TimeoutError
	if !errors.As(err, &to) || len(to.Pending) != 1 || to.Pending[0] != "a.x.io" {
		t.Fatalf("%T %v", err, err)
	}
}

func TestAnErrorBeforeTheDeadlineIsNotATimeout(t *testing.T) {
	fake := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{crdGVR: "CustomResourceDefinitionList"}, crdObj("a.x.io", true, nil))
	fake.PrependReactor("get", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	err := (&Client{Dynamic: fake}).WaitCRDsDeleted(context.Background(), []string{"a.x.io"}, time.Minute)
	var to *TimeoutError
	if err == nil || errors.As(err, &to) {
		t.Fatalf("%v", err)
	}
}

func TestRESTConfigRaisesClientSideRateLimits(t *testing.T) {
	// regression: client-go's default 5 QPS / burst 10 made 89 sequential dry-run applies take ~20s
	kc := t.TempDir() + "/kc"
	if err := os.WriteFile(kc, []byte("apiVersion: v1\nkind: Config\ncurrent-context: c\nclusters: [{name: c, cluster: {server: 'https://127.0.0.1:1'}}]\ncontexts: [{name: c, context: {cluster: c, user: u}}]\nusers: [{name: u, user: {token: t}}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kc)
	cfg, err := RESTConfig("", "test")
	if err != nil || cfg.QPS < 50 || cfg.Burst < 100 {
		t.Fatalf("%v %+v", err, cfg)
	}
}

func TestEstablishedTimeoutSaysWhyTheCRDIsNotAccepted(t *testing.T) {
	// regression: "timed out after 3s: certificates.cert-manager.io" left the operator to dig the
	// reason (another CRD owning the same kind) out of the CRD's status
	b := crdObj("b.x.io", false, nil)
	_ = unstructured.SetNestedSlice(b.Object, []interface{}{
		map[string]interface{}{"type": "NamesAccepted", "status": "False", "reason": "ListKindConflict", "message": "\"CertificateList\" is already in use"},
		map[string]interface{}{"type": "Established", "status": "False", "reason": "NotAccepted"},
	}, "status", "conditions")
	err := crdClient(b).WaitCRDsEstablished(context.Background(), []string{"b.x.io"}, 30*time.Millisecond)
	var to *TimeoutError
	if !errors.As(err, &to) || to.Pending[0] != "b.x.io" || !strings.Contains(err.Error(), "b.x.io (NamesAccepted=False ListKindConflict: \"CertificateList\" is already in use)") {
		t.Fatalf("%v", err)
	}
}
