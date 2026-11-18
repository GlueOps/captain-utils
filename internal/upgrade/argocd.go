package upgrade

import (
	"context"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/kube"
)

// The argocd release.
const (
	ArgocdRelease   = "argocd"
	ArgocdNamespace = "glueops-core"
	ArgocdChart     = "argo/argo-cd"
	ArgocdRepo      = "argo"
	// LegacyCRDsFieldManager owns the ArgoCD CRDs the legacy path applies.
	LegacyCRDsFieldManager = "captain-utils"
	skipCRDs               = "Skip"
)

// ArgocdOptions select what the flow does without asking.
type ArgocdOptions struct {
	Version        string // argo-cd chart version; empty = the pin (production) or a chooser
	CRDVersion     string // legacy path: ArgoCD app version whose CRDs to apply
	SkipCRDInstall bool   // legacy path: apply no CRDs
	DiffOnly       bool
}

// LegacyCRDBase is where the legacy path reads ArgoCD's CRD kustomization.
var LegacyCRDBase = "https://raw.githubusercontent.com/argoproj/argo-cd/%s/manifests/crds/"

// Argocd is the argocd menu item. ArgoCD's CRDs come from the platform-crds
// bundle on captains that pin platform_crds_version (the crds step, run before
// and after this one); others keep the legacy install of ArgoCD's own CRDs
// before the upgrade. The chart always runs with --skip-crds.
func Argocd(ctx context.Context, d *Deps, o ArgocdOptions, out io.Writer) error {
	if err := d.Helm.RepoUpdate(ctx, ArgocdRepo); err != nil {
		return err
	}
	var versions []string
	var pins captain.Versions
	if d.Production() {
		var err error
		if pins, err = d.Repo.GlueOpsVersions(); err != nil {
			return err
		}
		v, err := pins.Require(captain.ArgocdChartVersion)
		if err != nil {
			return err
		}
		versions = []string{v}
	}
	var index []helmx.ChartVersion
	if !d.Production() {
		var err error
		if index, err = d.Helm.SearchVersions(ArgocdRepo, "argo-cd", 30); err != nil {
			return err
		}
		for _, cv := range index {
			versions = append(versions, cv.Version)
		}
	}

	version := o.Version
	if version == "" {
		choice, err := d.choose("argo-cd chart version", "--version", append(versions, "Back")...)
		if err != nil || choice == "Back" {
			return err
		}
		version = choice
	}
	fmt.Fprintf(out, "chosen version: %s for %s\n", version, ArgocdChart)

	crdVersion := skipCRDs
	if d.Production() && d.Repo.CRDsBundleEnabled() {
		if o.CRDVersion != "" {
			return fmt.Errorf("--crd-version: this cluster is on the platform-crds bundle, which manages ArgoCD's CRDs (run crds before and after argocd)")
		}
		d.P.Info("ArgoCD CRDs are managed by the platform-crds bundle (menu item crds) — not applied by this step.")
	} else {
		var appVersions []string
		if d.Production() {
			appVersions = pins.All(captain.ArgocdAppVersion)
		} else {
			for _, cv := range index {
				if cv.Version == version {
					appVersions = append(appVersions, cv.AppVersion)
				}
			}
		}
		switch {
		case o.SkipCRDInstall, o.DiffOnly:
		case o.CRDVersion != "":
			crdVersion = o.CRDVersion
		default:
			d.P.Highlight("Select ArgoCD App Version (legacy CRD install — this cluster is not on the platform-crds bundle):")
			choice, err := d.choose("ArgoCD CRDs to apply", "--crd-version or --skip-crds-install", append(appVersions, skipCRDs, "Back")...)
			if err != nil || choice == "Back" {
				return err
			}
			crdVersion = choice
		}
	}

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
	if ok, err := d.confirm("Apply upgrade"); err != nil || !ok {
		return err
	}
	if crdVersion != skipCRDs {
		d.P.Highlight("Executing pre-commands for %s:", ArgocdRelease)
		if err := applyLegacyArgocdCRDs(ctx, d, crdVersion, out); err != nil {
			d.P.Alert("❌ Pre-commands failed. Aborting.")
			return err
		}
		if err := d.Helm.RepoUpdate(ctx, ArgocdRepo); err != nil {
			return err
		}
		d.P.Highlight("✅ Pre-commands complete.")
	}
	return d.upgrade(ctx, r, out)
}

// applyLegacyArgocdCRDs is what `kubectl apply -k
// https://github.com/argoproj/argo-cd/manifests/crds?ref=<ref>` did: it reads
// the kustomization at that ref, refuses anything but a plain resources list,
// and applies each file server-side.
func applyLegacyArgocdCRDs(ctx context.Context, d *Deps, ref string, out io.Writer) error {
	base := fmt.Sprintf(LegacyCRDBase, ref)
	d.P.Note("+ apply ArgoCD %s CRDs from %s (server-side, field manager %s)", ref, base, LegacyCRDsFieldManager)
	b, err := d.Fetch(ctx, base+"kustomization.yaml")
	if err != nil {
		return err
	}
	var k map[string]interface{}
	if err := yaml.Unmarshal(b, &k); err != nil {
		return fmt.Errorf("argo-cd %s kustomization.yaml: %w", ref, err)
	}
	var files []string
	for key, v := range k {
		switch key {
		case "apiVersion", "kind":
		case "resources":
			list, ok := v.([]interface{})
			if !ok {
				return fmt.Errorf("argo-cd %s kustomization.yaml: resources is not a list", ref)
			}
			for _, f := range list {
				s, ok := f.(string)
				if !ok || strings.Contains(s, "..") || strings.Contains(s, "://") || !strings.HasSuffix(s, ".yaml") {
					return fmt.Errorf("argo-cd %s kustomization.yaml: unsupported resource %v", ref, f)
				}
				files = append(files, s)
			}
		default:
			return fmt.Errorf("argo-cd %s kustomization.yaml uses %q, which captain-utils does not render; apply it with kubectl apply -k", ref, key)
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("argo-cd %s kustomization.yaml lists no resources", ref)
	}
	for _, f := range files {
		body, err := d.Fetch(ctx, base+f)
		if err != nil {
			return err
		}
		objs, err := kube.Objects(body)
		if err != nil {
			return fmt.Errorf("argo-cd %s %s: %w", ref, f, err)
		}
		for _, obj := range objs {
			if obj.GetKind() != "CustomResourceDefinition" {
				return fmt.Errorf("argo-cd %s %s: %s/%s is not a CRD, refusing", ref, f, obj.GetKind(), obj.GetName())
			}
			if _, err := d.Kube.Apply(ctx, obj, kube.ApplyOptions{FieldManager: LegacyCRDsFieldManager, Force: true}); err != nil {
				return fmt.Errorf("apply %s: %w", obj.GetName(), err)
			}
			fmt.Fprintf(out, "customresourcedefinition.apiextensions.k8s.io/%s serverside-applied\n", obj.GetName())
		}
	}
	return nil
}
