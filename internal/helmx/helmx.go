// Package helmx is captain_utils' Helm, through the Helm SDK instead of the
// helm CLI and its helm-diff plugin. It reads the same settings the CLI does
// (HELM_* environment, ~/.config/helm/repositories.yaml, the repository cache),
// so the codespace's configured repos (argo, glueops-platform, ...) resolve the
// same way.
//
// Release changes mirror `helm upgrade --install` and diffs mirror
// `helm diff upgrade --allow-unreleased` (helm-diff's own manifest and diff
// packages render them), so the operator sees what they saw before.
package helmx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/databus23/helm-diff/v3/diff"
	"github.com/databus23/helm-diff/v3/manifest"
	"github.com/mgutz/ansi"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/cli/values"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// DefaultTimeout is helm upgrade's own default (--timeout 5m0s).
const DefaultTimeout = 5 * time.Minute

// Release is one `helm upgrade --install` (or its diff).
type Release struct {
	Name            string
	Namespace       string
	Chart           string // repo/name, oci://..., or a local directory
	Version         string // empty for a local directory
	ValueFiles      []string
	SkipCRDs        bool
	CreateNamespace bool
	Description     string
}

// Command is the helm command line equivalent to an upgrade, for the transcript.
func (r Release) Command() string {
	args := []string{"helm", "upgrade", "--install", r.Name, r.Chart}
	if r.Version != "" {
		args = append(args, "--version", r.Version)
	}
	for _, f := range r.ValueFiles {
		args = append(args, "-f", f)
	}
	args = append(args, "-n", r.Namespace)
	if r.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	if r.SkipCRDs {
		args = append(args, "--skip-crds")
	}
	if r.Description != "" {
		args = append(args, "--description", fmt.Sprintf("%q", r.Description))
	}
	return strings.Join(args, " ")
}

// Client runs Helm actions against one kube context.
type Client struct {
	Settings *cli.EnvSettings
	Log      io.Writer // progress lines (repo updates, release summaries)
}

// New reads Helm's settings from the environment, as the CLI does.
func New(kubeContext string, log io.Writer) *Client {
	s := cli.New()
	if kubeContext != "" {
		s.KubeContext = kubeContext
	}
	return &Client{Settings: s, Log: log}
}

func (c *Client) config(namespace string) (*action.Configuration, error) {
	cfg := new(action.Configuration)
	s := *c.Settings
	s.SetNamespace(namespace)
	if err := cfg.Init(s.RESTClientGetter(), namespace, os.Getenv("HELM_DRIVER"), func(string, ...interface{}) {}); err != nil {
		return nil, fmt.Errorf("helm: %w", err)
	}
	rc, err := registry.NewClient(registry.ClientOptCredentialsFile(c.Settings.RegistryConfig), registry.ClientOptWriter(io.Discard))
	if err != nil {
		return nil, fmt.Errorf("helm registry client: %w", err)
	}
	cfg.RegistryClient = rc
	return cfg, nil
}

// repoFile loads repositories.yaml; a missing file is an empty list, as for the CLI.
func (c *Client) repoFile() (*repo.File, error) {
	f, err := repo.LoadFile(c.Settings.RepositoryConfig)
	if errors.Is(err, os.ErrNotExist) {
		return repo.NewFile(), nil
	}
	return f, err
}

// RepoUpdate refreshes the index of the named repositories (all when none are
// named), in parallel, like `helm repo update`.
func (c *Client) RepoUpdate(ctx context.Context, names ...string) error {
	f, err := c.repoFile()
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var entries []*repo.Entry
	found := map[string]bool{}
	for _, e := range f.Repositories {
		if len(want) == 0 || want[e.Name] {
			entries = append(entries, e)
			found[e.Name] = true
		}
	}
	for _, n := range names {
		if !found[n] {
			return fmt.Errorf("helm repository %q is not configured (%s)", n, c.Settings.RepositoryConfig)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed []string
	for _, e := range entries {
		wg.Add(1)
		go func(e *repo.Entry) {
			defer wg.Done()
			err := c.downloadIndex(e)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Fprintf(c.Log, "...Unable to get an update from the %q chart repository (%s):\n\t%v\n", e.Name, e.URL, err)
				failed = append(failed, e.Name)
				return
			}
			fmt.Fprintf(c.Log, "...Successfully got an update from the %q chart repository\n", e.Name)
		}(e)
	}
	wg.Wait()
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("failed to update the helm repositories: %s", strings.Join(failed, ", "))
	}
	return nil
}

func (c *Client) downloadIndex(e *repo.Entry) error {
	r, err := repo.NewChartRepository(e, getter.All(c.Settings))
	if err != nil {
		return err
	}
	r.CachePath = c.Settings.RepositoryCache
	_, err = r.DownloadIndexFile()
	return err
}

// RepoAdd adds a repository (a no-op when it is already there with the same
// URL), downloads its index and saves repositories.yaml, like `helm repo add`.
func (c *Client) RepoAdd(name, url string) error {
	f, err := c.repoFile()
	if err != nil {
		return err
	}
	if e := f.Get(name); e != nil {
		if e.URL != url {
			return fmt.Errorf("helm repository %q already exists with URL %s, not %s", name, e.URL, url)
		}
		fmt.Fprintf(c.Log, "%q already exists with the same configuration, skipping\n", name)
		return nil
	}
	e := &repo.Entry{Name: name, URL: url}
	if err := c.downloadIndex(e); err != nil {
		return fmt.Errorf("looks like %q is not a valid chart repository or cannot be reached: %w", url, err)
	}
	f.Update(e)
	if err := f.WriteFile(c.Settings.RepositoryConfig, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(c.Log, "%q has been added to your repositories\n", name)
	return nil
}

func (c *Client) loadChart(r Release, opts *action.ChartPathOptions) (*chart.Chart, error) {
	opts.Version = r.Version
	path, err := opts.LocateChart(r.Chart, c.Settings)
	if err != nil {
		return nil, err
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, err
	}
	if req := ch.Metadata.Dependencies; req != nil {
		if err := action.CheckDependencies(ch, req); err != nil {
			return nil, fmt.Errorf("chart %s: %w (run helm dependency build)", r.Chart, err)
		}
	}
	return ch, nil
}

func (c *Client) values(r Release) (map[string]interface{}, error) {
	return (&values.Options{ValueFiles: r.ValueFiles}).MergeValues(getter.All(c.Settings))
}

// exists reports whether the release has a revision that is not uninstalled.
func exists(cfg *action.Configuration, name string) (bool, error) {
	h := action.NewHistory(cfg)
	h.Max = 1
	versions, err := h.Run(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(versions) > 0 && versions[len(versions)-1].Info.Status != release.StatusUninstalled, nil
}

// DiffOptions control diff output.
type DiffOptions struct {
	Color bool
}

// Diff writes what `helm diff upgrade --allow-unreleased` would print to w and
// reports whether anything changes. The current side is the release's manifest
// plus its hooks; the new side is the chart rendered as `helm template
// --validate [--is-upgrade]` renders it. Test hooks are left out of both.
func (c *Client) Diff(ctx context.Context, r Release, opts DiffOptions, w io.Writer) (bool, error) {
	cfg, err := c.config(r.Namespace)
	if err != nil {
		return false, err
	}
	var current []byte
	cur, err := action.NewGet(cfg).Run(r.Name)
	isUpgrade := true
	switch {
	case errors.Is(err, driver.ErrReleaseNotFound):
		isUpgrade = false
	case err != nil:
		return false, fmt.Errorf("get release %s in namespace %s: %w", r.Name, r.Namespace, err)
	default:
		current = releaseManifest(cur)
	}

	inst := action.NewInstall(cfg)
	inst.ReleaseName = r.Name
	inst.Namespace = r.Namespace
	inst.DryRun = true
	inst.DryRunOption = "client"
	inst.Replace = true
	inst.ClientOnly = false // --validate: render against this cluster's API versions
	inst.IsUpgrade = isUpgrade
	inst.SkipCRDs = true
	ch, err := c.loadChart(r, &inst.ChartPathOptions)
	if err != nil {
		return false, fmt.Errorf("Failed to render chart: %w", err)
	}
	vals, err := c.values(r)
	if err != nil {
		return false, err
	}
	rendered, err := inst.RunWithContext(ctx, ch, vals)
	if err != nil {
		return false, fmt.Errorf("Failed to render chart: %w", err)
	}
	newManifest := templateOutput(rendered)

	ansi.DisableColors(!opts.Color)
	oldSpecs := map[string]*manifest.MappingResult{}
	if isUpgrade {
		oldSpecs = manifest.Parse(current, r.Namespace, false, manifest.Helm3TestHook, manifest.Helm2TestSuccessHook)
	}
	newSpecs := manifest.Parse(newManifest, r.Namespace, false, manifest.Helm3TestHook, manifest.Helm2TestSuccessHook)
	changed := diff.ManifestsOwnership(oldSpecs, newSpecs, nil, &diff.Options{OutputFormat: "diff", OutputContext: -1}, w)
	return changed, nil
}

// releaseManifest is `helm get manifest` followed by `helm get hooks`.
func releaseManifest(rel *release.Release) []byte {
	var b bytes.Buffer
	fmt.Fprintln(&b, rel.Manifest)
	for _, h := range rel.Hooks {
		fmt.Fprintf(&b, "---\n# Source: %s\n%s\n", h.Path, h.Manifest)
	}
	return b.Bytes()
}

// templateOutput is what `helm template` prints for a rendered release.
func templateOutput(rel *release.Release) []byte {
	var b bytes.Buffer
	fmt.Fprintln(&b, strings.TrimSpace(rel.Manifest))
	for _, h := range rel.Hooks {
		fmt.Fprintf(&b, "---\n# Source: %s\n%s\n", h.Path, h.Manifest)
	}
	return b.Bytes()
}

// Result summarises a release change.
type Result struct {
	Name, Namespace, Status, Chart string
	Revision                       int
	Installed                      bool
	Notes                          string
}

// Upgrade runs `helm upgrade --install`: an install when the release does not
// exist (or was uninstalled), an upgrade otherwise.
func (c *Client) Upgrade(ctx context.Context, r Release) (*Result, error) {
	cfg, err := c.config(r.Namespace)
	if err != nil {
		return nil, err
	}
	vals, err := c.values(r)
	if err != nil {
		return nil, err
	}
	found, err := exists(cfg, r.Name)
	if err != nil {
		return nil, err
	}
	var rel *release.Release
	if !found {
		fmt.Fprintf(c.Log, "Release %q does not exist. Installing it now.\n", r.Name)
		inst := action.NewInstall(cfg)
		inst.ReleaseName = r.Name
		inst.Namespace = r.Namespace
		inst.CreateNamespace = r.CreateNamespace
		inst.SkipCRDs = r.SkipCRDs
		inst.Description = r.Description
		inst.Timeout = DefaultTimeout
		ch, err := c.loadChart(r, &inst.ChartPathOptions)
		if err != nil {
			return nil, err
		}
		rel, err = inst.RunWithContext(ctx, ch, vals)
		if err != nil {
			return nil, fmt.Errorf("INSTALLATION FAILED: %w", err)
		}
	} else {
		up := action.NewUpgrade(cfg)
		up.Install = true
		up.Namespace = r.Namespace
		up.SkipCRDs = r.SkipCRDs
		up.Description = r.Description
		up.Timeout = DefaultTimeout
		ch, err := c.loadChart(r, &up.ChartPathOptions)
		if err != nil {
			return nil, err
		}
		rel, err = up.RunWithContext(ctx, r.Name, ch, vals)
		if err != nil {
			return nil, fmt.Errorf("UPGRADE FAILED: %w", err)
		}
	}
	res := &Result{Name: rel.Name, Namespace: rel.Namespace, Revision: rel.Version, Installed: !found}
	if rel.Info != nil {
		res.Status = rel.Info.Status.String()
		res.Notes = rel.Info.Notes
	}
	if rel.Chart != nil && rel.Chart.Metadata != nil {
		res.Chart = rel.Chart.Metadata.Name + "-" + rel.Chart.Metadata.Version
	}
	return res, nil
}
