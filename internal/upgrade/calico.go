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

// Calico is the aws → calico menu item: remove the EKS aws-node daemonset and
// install calico at the pinned chart version. It now asks first (the bash
// version deleted the daemonset without a prompt).
func Calico(ctx context.Context, d *Deps, out io.Writer) error {
	pins, err := d.Repo.GlueOpsVersions()
	if err != nil {
		return err
	}
	version, err := pins.Require(captain.CalicoChartVersion)
	if err != nil {
		return err
	}
	if ok, err := d.confirm("Remove the aws-node daemonset (kube-system) and install calico " + version + "?"); err != nil || !ok {
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
	if err := d.Helm.RepoAdd(CalicoRepo, CalicoRepoURL); err != nil {
		return err
	}
	if err := d.Helm.RepoUpdate(ctx, CalicoRepo); err != nil {
		return err
	}
	return d.upgrade(ctx, helmx.Release{
		Name: CalicoRelease, Namespace: CalicoNamespace, Chart: CalicoChart, Version: version,
		ValueFiles: []string{d.Repo.Path("calico.yaml")}, CreateNamespace: true,
	}, out)
}
