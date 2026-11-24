package upgrade

import (
	"context"
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
// install calico. Nothing is deleted or installed before that yes, and nothing at all if the
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
	if err := d.showDiff(ctx, r, false, out); err != nil {
		return err
	}
	if err := d.confirm("Remove the aws-node daemonset (kube-system) and apply this calico " + version + " upgrade?"); err != nil {
		return err
	}
	d.P.Alert("Removing eks daemonset")
	d.P.Note("+ kubectl delete daemonset -n kube-system aws-node")
	switch deleted, err := d.Kube.DeleteDaemonSet(ctx, "kube-system", "aws-node"); {
	case err != nil:
		d.P.Warn("could not delete daemonset kube-system/aws-node (continuing, as before): %v", err)
	case deleted:
		d.P.Plain(`daemonset.apps "aws-node" deleted`)
	default:
		d.P.Note("no aws-node daemonset in kube-system (already removed)")
	}
	d.P.Alert("Deploying calico helm chart %s", version)
	return d.upgrade(ctx, r, out)
}
