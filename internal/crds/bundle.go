// Package crds is the GlueOps layer-0 CRD bundle (GlueOps/platform-crds):
// diff -> confirm -> server-side apply, and the read-only check. It is the port
// of the codespace's libexec/captain_utils/crds command.
//
// Contract (kept from the bash command; tests guard each point):
//  1. CRDs are only ever server-side applied (force-conflicts, one field
//     manager). Never replace or update: replace erased metadata.finalizers,
//     destroying a terminating CRD and orphaning its custom resources.
//  2. The one non-dry-run Apply is doApply, and it needs a clearance, a value
//     only clear can produce, for the same live snapshot. Check never reaches
//     either of them, so it cannot change the cluster.
//  3. The plan block prints unconditionally, before the diff branch: the
//     no-diff path has no confirm and still strips.
//  4. Check exits 1 for drift only and 2 for any failure. Apply returns nil
//     on decline.
//  5. The command writes only after an explicit yes (or --yes). A clean diff
//     is not a licence to apply.
package crds

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/kube"
)

// Defaults.
const (
	FieldManager = "glueops-platform-crds"
	// DefaultChart mirrors ghcr.io/glueops/platform-crds.
	DefaultChart = "oci://ghcr.repo.gpkg.io/glueops/platform-crds"
	DefaultWait  = 120 * time.Second
	// GitHubRepo is where dev mode lists releases.
	GitHubRepo = "GlueOps/platform-crds"

	crdKind        = "CustomResourceDefinition"
	argoTrackingID = "argocd.argoproj.io/tracking-id"
	argoInstance   = "argocd.argoproj.io/instance"
	localVersion   = "local"
)

// bundle is the validated, parsed CRD set one run works on.
type bundle struct {
	objs  []*unstructured.Unstructured
	names []string
	union int
}

func (b *bundle) total() int { return len(b.names) }

// validVersion rejects empty or garbage: an empty --version makes helm pull LATEST.
func validVersion(v string) error {
	if !captain.IsReleaseTag(v) {
		return fmt.Errorf("platform_crds_version must be a release tag vX.Y.Z (got '%s') — check VERSIONS/glueops.yaml", v)
	}
	return nil
}

// parseBundle is the checks crds_fetch ran on the rendered bundle: it has CRDs,
// every CRD is its own document (a crds/*.yaml without a leading --- merges into
// its predecessor, and one mangled CRD would be applied), and nothing but CRDs.
func parseBundle(label string, cb *helmx.CRDBundle) (*bundle, error) {
	kinds := helmx.CountCRDKinds(cb.Manifest)
	if kinds == 0 {
		return nil, fmt.Errorf("platform-crds %s rendered no CRDs", label)
	}
	objs, err := kube.Objects(cb.Manifest)
	if err != nil {
		return nil, fmt.Errorf("platform-crds %s: bundle does not parse as %d separate CRD documents (every crds/*.yaml must start with --- — run hack/render.sh): %w", label, kinds, err)
	}
	var other []string
	b := &bundle{union: cb.Union}
	for _, o := range objs {
		if o.GetKind() != crdKind {
			other = append(other, o.GetKind()+"/"+o.GetName())
			continue
		}
		b.objs = append(b.objs, o)
		b.names = append(b.names, o.GetName())
	}
	if b.total() != kinds {
		return nil, fmt.Errorf("platform-crds %s: bundle does not parse as %d separate CRD documents (every crds/*.yaml must start with --- — run hack/render.sh)", label, kinds)
	}
	if len(other) > 0 {
		return nil, fmt.Errorf("platform-crds %s: bundle contains non-CRD objects, refusing to apply: %s", label, strings.Join(other, " "))
	}
	if b.union < b.total() {
		b.union = b.total()
	}
	return b, nil
}

// scope is "83 of 85 CRDs — ..." when profiles exclude some, else "83 CRDs".
func (b *bundle) scope() string {
	if b.union > b.total() {
		return fmt.Sprintf("%d of %d CRDs — profiles not enabled on this cluster are excluded", b.total(), b.union)
	}
	return fmt.Sprintf("%d CRDs", b.total())
}

// checkDir validates a local platform-crds checkout. strict (apply) also needs
// crds/*.yaml, each starting with --- (helm concatenates them without separators).
func checkDir(dir string, strict bool) (abs string, version string, files int, err error) {
	if dir == "" {
		return "", "", 0, fmt.Errorf("'%s' is not a directory", dir)
	}
	if fi, e := os.Stat(dir); e != nil || !fi.IsDir() {
		return "", "", 0, fmt.Errorf("'%s' is not a directory", dir)
	}
	abs, e := filepath.Abs(dir)
	if e != nil {
		return "", "", 0, fmt.Errorf("cannot enter '%s'", dir)
	}
	raw, e := os.ReadFile(filepath.Join(abs, "Chart.yaml"))
	if e != nil {
		return "", "", 0, fmt.Errorf("'%s' is not a rendered platform-crds checkout (needs Chart.yaml%s)", abs, map[bool]string{true: " and crds/*.yaml — run hack/render.sh", false: ""}[strict])
	}
	var meta struct{ Name, Version string }
	if e := yaml.Unmarshal(raw, &meta); e != nil {
		return "", "", 0, fmt.Errorf("%s/Chart.yaml: %w", abs, e)
	}
	crdFiles, _ := filepath.Glob(filepath.Join(abs, "crds", "*.yaml"))
	sub, _ := filepath.Glob(filepath.Join(abs, "charts", "*", "crds", "*.yaml"))
	all := append(crdFiles, sub...)
	if strict {
		if len(crdFiles) == 0 {
			return "", "", 0, fmt.Errorf("'%s' is not a rendered platform-crds checkout (needs Chart.yaml and crds/*.yaml — run hack/render.sh)", abs)
		}
		for _, f := range all {
			b, e := os.ReadFile(f)
			if e != nil {
				return "", "", 0, e
			}
			if !strings.HasPrefix(string(b), "---\n") && string(b) != "---" {
				return "", "", 0, fmt.Errorf("%s must start with --- (helm show crds concatenates crds/*.yaml without separators — run hack/render.sh)", f)
			}
		}
	}
	if meta.Name != "platform-crds" {
		return "", "", 0, fmt.Errorf("'%s/Chart.yaml' is chart '%s', not platform-crds", abs, meta.Name)
	}
	return abs, meta.Version, len(all), nil
}

// liveOf selects the live objects that are in the bundle.
func liveOf(b *bundle, all []*unstructured.Unstructured) []*unstructured.Unstructured {
	want := map[string]bool{}
	for _, n := range b.names {
		want[n] = true
	}
	var out []*unstructured.Unstructured
	for _, o := range all {
		if want[o.GetName()] {
			out = append(out, o)
		}
	}
	return out
}

// readLive lists the live CRDs of the bundle (one API call).
func readLive(ctx context.Context, cl Cluster, b *bundle) ([]*unstructured.Unstructured, error) {
	all, err := cl.ListCRDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list CRDs on the current kube context: %w", err)
	}
	return liveOf(b, all), nil
}
