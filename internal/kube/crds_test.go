package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
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
