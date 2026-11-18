package kube

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// helmSecret builds a release Secret the way Helm's secrets driver stores it.
func helmSecret(ns, name string, rev int, status, chartVersion string) *corev1.Secret {
	js := fmt.Sprintf(`{"name":%q,"namespace":%q,"version":%d,"chart":{"metadata":{"name":"c","version":%q}}}`, name, ns, rev, chartVersion)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte(js))
	_ = w.Close()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, rev),
			Labels: map[string]string{"owner": "helm", "name": name, "status": status, "version": fmt.Sprint(rev)},
		},
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))},
	}
}

func TestReleaseChartVersion(t *testing.T) {
	c := &Client{Kube: fake.NewSimpleClientset(
		helmSecret("glueops-core", "argocd", 1, "superseded", "9.0.0"),
		helmSecret("glueops-core", "argocd", 2, "deployed", "9.3.7"),
		helmSecret("glueops-core", "argocd", 3, "pending-upgrade", "9.4.0"),
		helmSecret("tigera-operator", "calico", 4, "failed", "v3.31.4"),
	)}
	ctx := context.Background()
	for release, want := range map[string]string{"argocd": "9.3.7", "calico": "v3.31.4", "glueops-platform": ""} {
		got, err := c.ReleaseChartVersion(ctx, release)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", release, got, err, want)
		}
	}
}

func TestDecodeReleaseUncompressed(t *testing.T) {
	r, err := DecodeRelease([]byte(base64.StdEncoding.EncodeToString([]byte(`{"name":"x","chart":{"metadata":{"version":"1.2.3"}}}`))))
	if err != nil || r.Chart.Metadata.Version != "1.2.3" {
		t.Fatalf("%+v, %v", r, err)
	}
}

func TestServerVersion(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "34+", GitVersion: "v1.34.2-eks-113cf36"}
	got, err := (&Client{Kube: cs}).ServerVersion(context.Background())
	if err != nil || got != "1.34" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestDeploymentReads(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tigera-operator", Name: "tigera-operator", Labels: map[string]string{"app.kubernetes.io/version": "v3.2.12"}},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "quay.io/tigera/operator:v1.40.7"}}}}},
	}
	c := &Client{Kube: fake.NewSimpleClientset([]runtime.Object{d}...)}
	ctx := context.Background()
	if img, err := c.DeploymentImage(ctx, "tigera-operator", "tigera-operator"); err != nil || img != "quay.io/tigera/operator:v1.40.7" {
		t.Errorf("image %q, %v", img, err)
	}
	if l, err := c.DeploymentLabel(ctx, "tigera-operator", "tigera-operator", "app.kubernetes.io/version"); err != nil || l != "v3.2.12" {
		t.Errorf("label %q, %v", l, err)
	}
	if l, err := c.DeploymentLabel(ctx, "glueops-core", "argocd-server", "x"); err != nil || l != "" {
		t.Errorf("missing deployment: %q, %v", l, err)
	}
}
