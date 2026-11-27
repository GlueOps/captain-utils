package upgrade

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
)

// The glueops-platform release.
const (
	PlatformRelease   = "glueops-platform"
	PlatformNamespace = "glueops-core"
	PlatformChart     = "glueops-platform/glueops-platform"
	PlatformRepo      = "glueops-platform"
)

// PlatformOptions select what the flow does without asking.
type PlatformOptions struct {
	Version  string // chart version; empty = a chooser offering the pin (needs a terminal)
	ChartDir string // "custom": a local chart directory instead of a version
	DiffOnly bool   // stop after the diff (print it, change nothing)
}

// Platform is the glueops-platform menu item: choose a version (or custom),
// diff, confirm, upgrade --install.
func Platform(ctx context.Context, d *Deps, o PlatformOptions, out io.Writer) error {
	overrides := "platform.yaml"
	if captain.Exists(d.Repo.Path("overrides.yaml")) {
		d.P.Highlight("Overrides.yaml detected")
		overrides = "overrides.yaml"
	} else {
		d.P.Alert("No Overrides.yaml detected")
	}
	if o.ChartDir != "" {
		return platformCustom(ctx, d, o.ChartDir, overrides, o.DiffOnly, out)
	}

	version := o.Version
	if version == "" {
		if !d.Prompt.Interactive() {
			return errNeedFlag("choose", "--version or --chart-dir")
		}
		pins, err := d.Repo.GlueOpsVersions()
		if err != nil {
			return err
		}
		pin, err := pins.Require(captain.PlatformChartVersion)
		if err != nil {
			return err
		}
		choice, err := d.choose("glueops-platform version", "--version or --chart-dir", pin, "custom", "Back")
		if err != nil {
			return err
		}
		switch choice {
		case "Back":
			return nil
		case "custom":
			return platformCustom(ctx, d, "", overrides, o.DiffOnly, out)
		}
		version = choice
	}
	d.P.Info("chosen version: %s for %s", version, PlatformChart)

	if err := d.Helm.RepoUpdate(ctx, PlatformRepo); err != nil {
		return err
	}
	r := helmx.Release{
		Name: PlatformRelease, Namespace: PlatformNamespace, Chart: PlatformChart, Version: version,
		ValueFiles:      []string{d.Repo.Path("platform.yaml"), d.Repo.Path(overrides)},
		CreateNamespace: true,
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

// platformCustom installs glueops-platform from a local chart directory, stamped
// with --description "custom: <dir> (<branch>@<sha>[, dirty])" so helm history
// shows what is really installed. Refusals are errors; declining is not.
func platformCustom(ctx context.Context, d *Deps, dir, overrides string, diffOnly bool, out io.Writer) error {
	d.P.Alert("⚠️  custom installs an UNRELEASED chart on this cluster. The VERSIONS/glueops.yaml pin will no longer describe what is running, and show_diff_table will usually NOT show it (feature branches keep main's Chart.yaml version) — 'helm history %s -n %s' is the only record. Re-run glueops-platform with the pinned version to get back.", PlatformRelease, PlatformNamespace)
	if dir == "" {
		if !d.Prompt.Interactive() {
			return errNeedFlag("ask for the chart directory", "--chart-dir")
		}
		var err error
		dir, err = d.Prompt.Path("Local chart directory — a checkout of GlueOps/platform-helm-chart-platform (Tab completes, empty = back):", d.PathPrefill)
		if err != nil || dir == "" {
			return err
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("'%s' is not a directory", dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		return fmt.Errorf("'%s' has no Chart.yaml — point at a checkout of GlueOps/platform-helm-chart-platform", dir)
	}
	var meta struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(b, &meta); err != nil || meta.Name == "" {
		return fmt.Errorf("cannot read .name from %s/Chart.yaml", dir)
	}
	if meta.Version == "" {
		return fmt.Errorf("cannot read .version from %s/Chart.yaml", dir)
	}
	git := d.GitInfo(dir)
	src := dir
	if git != "" {
		src += " (" + git + ")"
	}
	d.P.Highlight("chart %s %s from %s", meta.Name, meta.Version, src)
	if meta.Name != PlatformRelease {
		d.P.Warn("note: Chart.yaml name is '%s' but it will be installed as release '%s' in %s", meta.Name, PlatformRelease, PlatformNamespace)
	}
	d.P.Plain("helm list will show %s-%s; the directory is recorded only in the release description: custom: %s", PlatformRelease, meta.Version, src)
	if !diffOnly {
		if err := d.confirm("Diff this chart against the cluster?"); err != nil {
			return err
		}
	}
	r := helmx.Release{
		Name: PlatformRelease, Namespace: PlatformNamespace, Chart: dir,
		ValueFiles:      []string{d.Repo.Path("platform.yaml"), d.Repo.Path(overrides)},
		CreateNamespace: true,
		Description:     "custom: " + src,
	}
	if err := d.showDiff(ctx, r, diffOnly, out); err != nil {
		return fmt.Errorf("helm diff failed: %w", err)
	}
	if diffOnly {
		return nil
	}
	if err := d.confirm("Apply upgrade"); err != nil {
		return err
	}
	if err := d.upgrade(ctx, r, out); err != nil {
		return fmt.Errorf("helm upgrade failed: %w", err)
	}
	d.P.Highlight("✅ %s installed from %s (helm history %s -n %s shows the description)", PlatformRelease, dir, PlatformRelease, PlatformNamespace)
	return nil
}
