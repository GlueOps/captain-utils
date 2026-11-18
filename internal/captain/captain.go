// Package captain reads the files of a captain repository: the VERSIONS pins
// and the paths the upgrade flows work on. It never writes.
package captain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// Pin names in VERSIONS/glueops.yaml.
const (
	PlatformChartVersion  = "glueops_platform_helm_chart_version"
	ArgocdChartVersion    = "argocd_helm_chart_version"
	ArgocdAppVersion      = "argocd_app_version"
	CodespaceVersion      = "codespace_version"
	CalicoChartVersion    = "calico_helm_chart_version"
	CalicoCtlVersion      = "calico_ctl_version"
	TigeraOperatorVersion = "tigera_operator_version"
	TerraformModuleVer    = "terraform_module_version"
	PlatformCRDsVersion   = "platform_crds_version"
)

// Pin names in VERSIONS/aws.yaml.
const (
	CSIDriverVersion  = "csi_driver_version"
	CoreDNSVersion    = "coredns_version"
	KubeProxyVersion  = "kube_proxy_version"
	AMIReleaseVersion = "ami_release_version"
	KubernetesVersion = "kubernetes_version"
)

// Repo is a captain repository checkout.
type Repo struct{ Dir string }

// Path joins elem onto the repo root.
func (r Repo) Path(elem ...string) string { return filepath.Join(append([]string{r.Dir}, elem...)...) }

// GlueOpsVersions reads VERSIONS/glueops.yaml.
func (r Repo) GlueOpsVersions() (Versions, error) {
	return ReadVersions(r.Path("VERSIONS", "glueops.yaml"))
}

// AWSVersions reads VERSIONS/aws.yaml.
func (r Repo) AWSVersions() (Versions, error) { return ReadVersions(r.Path("VERSIONS", "aws.yaml")) }

// MainTF is terraform/kubernetes/main.tf, the file the aws flows edit.
func (r Repo) MainTF() string { return r.Path("terraform", "kubernetes", "main.tf") }

// TFState is the local terraform state next to main.tf (absent with a remote backend).
func (r Repo) TFState() string { return r.Path("terraform", "kubernetes", "terraform.tfstate") }

// Pin is one {name, version} entry.
type Pin struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// Versions is a VERSIONS/*.yaml file, entries in file order.
type Versions struct {
	File string
	Pins []Pin
}

// ErrNotExist is returned (wrapped) when a VERSIONS file is missing.
var ErrNotExist = fs.ErrNotExist

// ReadVersions parses a VERSIONS file. Scalars are kept as written, so an
// unquoted 1.30 stays "1.30".
func ReadVersions(path string) (Versions, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Versions{}, err
	}
	var doc struct {
		Versions []Pin `yaml:"versions"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return Versions{}, fmt.Errorf("%s: %w", path, err)
	}
	return Versions{File: path, Pins: doc.Versions}, nil
}

// Get returns the first pin named name.
func (v Versions) Get(name string) (string, bool) {
	for _, p := range v.Pins {
		if p.Name == name {
			return p.Version, true
		}
	}
	return "", false
}

// All returns every pin named name (a duplicated pin is an error for callers
// that need exactly one).
func (v Versions) All(name string) []string {
	var out []string
	for _, p := range v.Pins {
		if p.Name == name {
			out = append(out, p.Version)
		}
	}
	return out
}

// Require returns the pin or an error naming the file.
func (v Versions) Require(name string) (string, error) {
	s, ok := v.Get(name)
	if !ok || s == "" {
		return "", fmt.Errorf("%s has no %s pin", v.File, name)
	}
	return s, nil
}

var releaseTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// IsReleaseTag reports whether s looks like vX.Y.Z (or X.Y.Z).
func IsReleaseTag(s string) bool { return releaseTag.MatchString(s) }

// CRDsBundleEnabled reports whether the cluster is on the platform-crds
// bundle: exactly one release-tag-shaped platform_crds_version pin.
func (r Repo) CRDsBundleEnabled() bool {
	v, err := r.GlueOpsVersions()
	if err != nil {
		return false
	}
	pins := v.All(PlatformCRDsVersion)
	return len(pins) == 1 && IsReleaseTag(pins[0])
}

// Exists reports whether path exists (errors other than not-exist count as existing).
func Exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
