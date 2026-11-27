package upgrade

import (
	"context"
	"fmt"
	"io"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
)

// The calico release.
const (
	CalicoRelease   = "calico"
	CalicoNamespace = "tigera-operator"
	CalicoChart     = "projectcalico/tigera-operator"
	CalicoRepo      = "projectcalico"
	CalicoRepoURL   = "https://docs.tigera.io/calico/charts"
)

// Calico is the aws → calico menu item: show the helm diff of the calico release at the pinned
// chart version, then ask once (naming both actions), remove the EKS aws-node daemonset and
// install calico (a first install has no diff to show). Nothing is deleted or installed before that yes, and nothing at all if the
// repo refresh or the diff fails. (The bash version showed no diff and deleted the daemonset
// without a prompt.)
func Calico(ctx context.Context, d *Deps, out io.Writer) error {
	pins, err := d.Repo.GlueOpsVersions()
	if err != nil {
		return err
	}
	version, err := pins.Require(captain.CalicoChartVersion)
	if err != nil {
		return err
	}
	if err := d.Helm.RepoAdd(CalicoRepo, CalicoRepoURL); err != nil {
		return err
	}
	if err := d.Helm.RepoUpdate(ctx, CalicoRepo); err != nil {
		return err
	}
	r := helmx.Release{
		Name: CalicoRelease, Namespace: CalicoNamespace, Chart: CalicoChart, Version: version,
		ValueFiles: []string{d.Repo.Path("calico.yaml")}, CreateNamespace: true,
	}
	// A first install has nothing to diff against, and validating the render against the cluster
	// fails before the chart's own crds/ (Installation, ...) exist: skip the diff there.
	installed, err := d.Helm.ReleaseExists(ctx, r)
	if err != nil {
		return err
	}
	verb := "upgrade"
	if installed {
		if err := d.showDiff(ctx, r, false, out); err != nil {
			return err
		}
	} else {
		verb = "install"
		d.Result.release(r)
		d.P.Note("no %s release in %s yet: no diff to show (this is a first install; the chart's crds/ are installed with it)", r.Name, r.Namespace)
	}
	if err := d.confirm("Remove the aws-node daemonset (kube-system) and apply this calico " + version + " " + verb + "?"); err != nil {
		return err
	}
	d.P.Alert("Removing eks daemonset")
	d.P.Note("+ kubectl delete daemonset -n kube-system aws-node")
	switch deleted, err := d.Kube.DeleteDaemonSet(ctx, "kube-system", "aws-node"); {
	case err != nil:
		d.P.Warn("could not delete daemonset kube-system/aws-node (continuing, as before): %v", err)
		d.Result.warn(fmt.Sprintf("could not delete daemonset kube-system/aws-node: %v", err))
	case deleted:
		d.P.Plain(`daemonset.apps "aws-node" deleted`)
	default:
		d.P.Note("no aws-node daemonset in kube-system (already removed)")
	}
	d.P.Alert("Deploying calico helm chart %s", version)
	if err := d.upgrade(ctx, r, out); err != nil {
		// aws-node is gone by now (unless it was already absent or its delete failed above)
		return fmt.Errorf("calico install failed after the aws-node daemonset was removed; re-run to finish: %w", err)
	}
	return nil
}
