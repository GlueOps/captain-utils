// Package upgrade holds the Helm release flows of the captain_utils menu:
// glueops-platform (pinned and "custom" local chart), argocd (with the legacy
// ArgoCD CRD install for captains not on the platform-crds bundle) and calico.
// Messages, prompts and their order are the bash menu's.
package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/kube"
	"github.com/glueops/captain-utils/internal/ui"
)

// Helm is what the flows need from helmx.Client.
type Helm interface {
	RepoUpdate(ctx context.Context, names ...string) error
	RepoAdd(name, url string) error
	SearchVersions(repo, chart string, max int) ([]helmx.ChartVersion, error)
	Diff(ctx context.Context, r helmx.Release, opts helmx.DiffOptions, w io.Writer) (bool, error)
	Upgrade(ctx context.Context, r helmx.Release) (*helmx.Result, error)
}

// Kube is what the flows need from kube.Client.
type Kube interface {
	DeleteDaemonSet(ctx context.Context, namespace, name string) (bool, error)
	Apply(ctx context.Context, obj *unstructured.Unstructured, o kube.ApplyOptions) (*unstructured.Unstructured, error)
}

// Releases lists GitHub release tags (dev mode's version lists).
type Releases interface {
	ReleaseTags(ctx context.Context, repo string, n int) ([]string, error)
}

// Deps is everything a flow uses.
type Deps struct {
	Helm    Helm
	Kube    Kube
	GitHub  Releases
	Prompt  ui.Prompter
	P       *ui.Printer
	Repo    captain.Repo
	Env     string // production | dev
	Yes     bool   // --yes: confirmations answer yes
	Color   bool   // colour the helm diff
	GitInfo func(dir string) string
	Fetch   func(ctx context.Context, url string) ([]byte, error)
	// PathPrefill starts the local-directory prompt (PLATFORM_CHART_DIR_PREFILL).
	PathPrefill string
}

// Production reports whether the flows read pins from VERSIONS/*.yaml.
func (d *Deps) Production() bool { return d.Env != "dev" }

// errNoTTY is returned when a flow needs an answer and has no terminal.
func errNoTTY(what string) error {
	return fmt.Errorf("no terminal to %s on: %w", what, ui.ErrNoTerminal)
}

func (d *Deps) confirm(title string) (bool, error) {
	if d.Yes {
		d.P.Note("%s yes (--yes)", title)
		return true, nil
	}
	if !d.Prompt.Interactive() {
		return false, fmt.Errorf("%s: %w (pass --yes to run without one)", title, ui.ErrNoTerminal)
	}
	return d.Prompt.Confirm(title)
}

func (d *Deps) choose(title, flag string, options ...string) (string, error) {
	if !d.Prompt.Interactive() {
		return "", fmt.Errorf("%w (pass %s)", errNoTTY("choose"), flag)
	}
	return d.Prompt.Choose(title, options...)
}

// showDiff renders the helm diff and pages it (or prints it with diffOnly).
func (d *Deps) showDiff(ctx context.Context, r helmx.Release, diffOnly bool, out io.Writer) error {
	var buf bytes.Buffer
	changed, err := d.Helm.Diff(ctx, r, helmx.DiffOptions{Color: d.Color}, &buf)
	if err != nil {
		return err
	}
	switch {
	case !changed:
		d.P.Note("helm diff: no changes for %s in %s", r.Name, r.Namespace)
	case diffOnly:
		if _, err := out.Write(buf.Bytes()); err != nil {
			return err
		}
	default:
		if err := d.Prompt.Pager(buf.String()); err != nil {
			return err
		}
	}
	d.P.Highlight("✅ Diff complete.")
	return nil
}

func (d *Deps) upgrade(ctx context.Context, r helmx.Release, out io.Writer) error {
	d.P.Highlight("The following commands will be executed:")
	d.P.Note("+ %s", r.Command())
	res, err := d.Helm.Upgrade(ctx, r)
	if err != nil {
		return err
	}
	verb := "upgraded"
	if res.Installed {
		verb = "installed"
	}
	fmt.Fprintf(out, "Release %q has been %s. Happy Helming!\nNAME: %s\nNAMESPACE: %s\nSTATUS: %s\nREVISION: %d\n", res.Name, verb, res.Name, res.Namespace, res.Status, res.Revision)
	if res.Notes != "" {
		fmt.Fprintf(out, "NOTES:\n%s\n", res.Notes)
	}
	return nil
}

// HTTPFetch GETs a URL (the legacy ArgoCD CRD install's raw.githubusercontent.com files).
func HTTPFetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// ErrBack means the operator backed out; callers treat it as success.
var ErrBack = errors.New("back")
