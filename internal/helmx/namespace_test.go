package helmx

import (
	"io"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// Objects a chart renders without metadata.namespace must land in the release's namespace.
// Regression: config() once set the namespace on a copy of the Settings, which the REST
// getter does not read, so `platform upgrade` created a chart's Applications in "default"
// while the release record said glueops-core.
func TestConfigKubeClientUsesReleaseNamespace(t *testing.T) {
	t.Setenv("KUBECONFIG", "/nonexistent")
	t.Setenv("HELM_NAMESPACE", "")
	c := New("", io.Discard)
	for _, ns := range []string{"glueops-core", "tigera-operator"} {
		cfg, err := c.config(ns)
		if err != nil {
			t.Fatal(err)
		}
		g, ok := cfg.RESTClientGetter.(interface {
			ToRawKubeConfigLoader() clientcmd.ClientConfig
		})
		if !ok {
			t.Fatalf("%T has no ToRawKubeConfigLoader", cfg.RESTClientGetter)
		}
		got, _, err := g.ToRawKubeConfigLoader().Namespace()
		if err != nil || got != ns {
			t.Errorf("namespace %q: client resolves %q (%v)", ns, got, err)
		}
	}
	// the shared settings are not changed by building a configuration
	if got := c.Settings.Namespace(); got == "tigera-operator" || got == "glueops-core" {
		t.Errorf("c.Settings namespace was changed to %q", got)
	}
}
