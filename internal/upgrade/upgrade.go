// Package upgrade holds the Helm release flows of the captain_utils menu:
// glueops-platform (pinned and "custom" local chart), argocd and calico. ArgoCD's
// CRDs come from the platform-crds bundle (the crds step), never from these flows.
// Messages, prompts and their order are the bash menu's.
package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/ui"
)

// Helm is what the flows need from helmx.Client.
type Helm interface {
	RepoUpdate(ctx context.Context, names ...string) error
	RepoAdd(name, url string) error
	Diff(ctx context.Context, r helmx.Release, opts helmx.DiffOptions, w io.Writer) (bool, error)
	Upgrade(ctx context.Context, r helmx.Release) (*helmx.Result, error)
}

// Kube is what the flows need from kube.Client.
type Kube interface {
	DeleteDaemonSet(ctx context.Context, namespace, name string) (bool, error)
}

// Deps is everything a flow uses.
type Deps struct {
	Helm    Helm
	Kube    Kube
	Prompt  ui.Prompter
	P       *ui.Printer
	Repo    captain.Repo
	Yes     bool // --yes: confirmations answer yes
	Color   bool // colour the helm diff
	GitInfo func(dir string) string
	// PathPrefill starts the local-directory prompt (PLATFORM_CHART_DIR_PREFILL).
	PathPrefill string
}

// errNeedFlag is returned when a flow needs a choice and has no terminal to ask on: the
// command line has to make it, so it is a usage error.
func errNeedFlag(what, flag string) error {
	return ui.Usage(fmt.Errorf("no terminal to %s on: %w (pass %s)", what, ui.ErrNoTerminal, flag))
}

// confirm asks title; no (or Esc) is ui.ErrDeclined, so a flow that was declined is not
// mistaken for one that ran. Only the --yes flag answers for the operator.
func (d *Deps) confirm(title string) error {
	if d.Yes {
		d.P.Note("%s yes (--yes)", title)
		return nil
	}
	if !d.Prompt.Interactive() {
		return fmt.Errorf("%s: %w (pass --yes to run without one)", title, ui.ErrNoTerminal)
	}
	ok, err := d.Prompt.Confirm(title)
	if err != nil {
		return err
	}
	if !ok {
		return ui.ErrDeclined
	}
	return nil
}

func (d *Deps) choose(title, flag string, options ...string) (string, error) {
	if !d.Prompt.Interactive() {
		return "", errNeedFlag("choose", flag)
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

// ErrBack means the operator backed out; callers treat it as success.
var ErrBack = errors.New("back")
