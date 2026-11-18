package aws

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func readLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return Lines(string(b))
}

func golden(t *testing.T, name string, got []string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, []byte(Join(got)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if Join(got) != string(want) {
		t.Fatalf("%s differs (go test ./internal/aws -update to rewrite):\n%s", name, Join(got))
	}
}

func TestLinesRoundTrip(t *testing.T) {
	for _, s := range []string{"", "a", "a\n", "a\nb", "a\n\nb\n"} {
		if got := Join(Lines(s)); got != s {
			t.Errorf("%q -> %q", s, got)
		}
	}
}

func TestUpgradeAddons(t *testing.T) {
	got, changes := UpgradeAddons(readLines(t, "main.tf"), map[string]string{
		"csi_driver_version": "v1.63.1-eksbuild.1",
		"coredns_version":    "v1.13.2-eksbuild.11",
		"kube_proxy_version": "", // no pin: left alone
	})
	golden(t, "main.tf.addons.golden", got)
	if len(changes) != 2 || changes[0].Key != "csi_driver_version" || changes[1].Old != "v1.11.4-eksbuild.14" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestUpgradeAddonsEmptyValue(t *testing.T) {
	// Python's str.replace("", v) put v between every character of the line
	got, _ := UpgradeAddons([]string{`  coredns_version = ""` + "\n"}, map[string]string{"coredns_version": "v1"})
	if got[0] != `  coredns_version = "v1"`+"\n" {
		t.Fatalf("got %q", got[0])
	}
}

func TestUpgradeKubernetes(t *testing.T) {
	got, changes := UpgradeKubernetes(readLines(t, "main.tf"), "1.34")
	golden(t, "main.tf.kubernetes.golden", got)
	if len(changes) != 1 || changes[0].Line != 4 || changes[0].Old != "1.33" {
		t.Fatalf("changes = %+v", changes)
	}
	// already at target: no change
	if _, c := UpgradeKubernetes(got, "1.34"); len(c) != 0 {
		t.Fatalf("second run changed %+v", c)
	}
}

func TestAddNodePools(t *testing.T) {
	got, changes, err := AddNodePools(readLines(t, "main.tf"), "1.34.9-20260801", "1.33")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "main.tf.nodepools.golden", got)
	var keys []string
	for _, c := range changes {
		keys = append(keys, c.Key+"="+c.New)
	}
	// the commented-out pool is copied too (as the Python helper did): its name and
	// kubernetes_version change, its ami does not (that regex never matched a commented line)
	want := "kubernetes_version=1.33 name=glueops-platform-node-pool-2 ami_release_version=1.34.9-20260801 kubernetes_version=1.33 name=glueops-platform-node-pool-3"
	if strings.Join(keys, " ") != want {
		t.Fatalf("changes:\n got %s\nwant %s", strings.Join(keys, " "), want)
	}
}

func TestAddNodePoolsBlankLineBeforePeering(t *testing.T) {
	in := Lines("node_pools = [\n  {\n    \"name\" : \"p-1\",\n  }\n]\n\npeering_configs = []\n")
	got, _, err := AddNodePools(in, "ami", "1.34")
	if err != nil {
		t.Fatal(err)
	}
	want := "node_pools = [\n  {\n    \"name\" : \"p-1\",\n  },\n  {\n    \"name\" : \"p-2\",\n  }\n]\n\npeering_configs = []\n"
	if Join(got) != want {
		t.Fatalf("got:\n%s", Join(got))
	}
}

func TestAddNodePoolsErrors(t *testing.T) {
	for name, in := range map[string]string{
		"no node_pools":   "peering_configs = []\n",
		"no peering":      "node_pools = [\n{\n}\n]\n",
		"empty list":      "node_pools = [\n]\npeering_configs = []\n",
		"no bracket":      "node_pools = [\n{\n}\npeering_configs = []\n",
		"name without -N": "node_pools = [\n{\n\"name\" : \"pool\",\n}\n]\npeering_configs = []\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := AddNodePools(Lines(in), "ami", "1.34"); err == nil {
				t.Fatal("want an error")
			}
		})
	}
	if _, _, err := AddNodePools(readLines(t, "main.tf"), "", "1.34"); err == nil {
		t.Fatal("empty ami must fail")
	}
	if _, _, err := AddNodePools(readLines(t, "main.tf"), "ami", ""); err == nil {
		t.Fatal("empty kubernetes version must fail")
	}
}

func TestCurrentValues(t *testing.T) {
	l := readLines(t, "main.tf")
	if got := CurrentAMI(l); got != "1.33.0-20250519" {
		t.Errorf("ami = %q", got)
	}
	if got := CurrentModuleRef(l); got != "v0.50.0" {
		t.Errorf("ref = %q", got)
	}
}

func TestStateAddonVersions(t *testing.T) {
	got, err := StateAddonVersions(filepath.Join("testdata", "terraform.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	if got["csi_driver_version"] != "v1.45.0-eksbuild.2" || got["coredns_version"] != "v1.11.4-eksbuild.14" || got["kube_proxy_version"] != "" {
		t.Fatalf("got %v", got)
	}
	if got, err := StateAddonVersions(filepath.Join(t.TempDir(), "none")); got != nil || err != nil {
		t.Fatalf("missing file: %v, %v", got, err)
	}
}
