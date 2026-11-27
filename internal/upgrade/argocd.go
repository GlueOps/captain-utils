package upgrade

import (
	"context"
	"io"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
)

// The argocd release.
const (
	ArgocdRelease   = "argocd"
	ArgocdNamespace = "glueops-core"
	ArgocdChart     = "argo/argo-cd"
	ArgocdRepo      = "argo"
)

// ArgocdOptions select what the flow does without asking.
type ArgocdOptions struct {
	Version  string // argo-cd chart version; empty = a chooser offering the pin (needs a terminal)
	DiffOnly bool
}

// Argocd is the argocd menu item: choose the version, diff, confirm, upgrade --install.
// ArgoCD's CRDs come from the platform-crds bundle (the crds step, run before and after
// this one); the chart always runs with --skip-crds and this flow applies no CRDs.
func Argocd(ctx context.Context, d *Deps, o ArgocdOptions, out io.Writer) error {
	version := o.Version
	if version == "" {
		if !d.Prompt.Interactive() {
			return errNeedFlag("choose", "--version")
		}
		pins, err := d.Repo.GlueOpsVersions()
		if err != nil {
			return err
		}
		pin, err := pins.Require(captain.ArgocdChartVersion)
		if err != nil {
			return err
		}
		choice, err := d.choose("argo-cd chart version", "--version", pin, "Back")
		if err != nil || choice == "Back" {
			return err
		}
		version = choice
	}
	if err := d.Helm.RepoUpdate(ctx, ArgocdRepo); err != nil {
		return err
	}
	d.P.Info("chosen version: %s for %s", version, ArgocdChart)
	d.P.Info("ArgoCD CRDs are managed by the platform-crds bundle (menu item crds) — not applied by this step.")

	r := helmx.Release{
		Name: ArgocdRelease, Namespace: ArgocdNamespace, Chart: ArgocdChart, Version: version,
		ValueFiles: []string{d.Repo.Path("argocd.yaml")}, CreateNamespace: true, SkipCRDs: true,
	}
	if err := d.showDiff(ctx, r, o.DiffOnly, out); err != nil {
		return err
	}
	if o.DiffOnly {
		return nil
	}
	if err := d.confirm("Apply upgrade"); err != nil {
		return err
	}
	return d.upgrade(ctx, r, out)
}
