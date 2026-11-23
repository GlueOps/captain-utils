package cmd

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/glueops/captain-utils/internal/crds"
	"github.com/glueops/captain-utils/internal/gitinfo"
	"github.com/glueops/captain-utils/internal/ui"
)

// crdsDeps builds the crds flows' dependencies.
func (a *App) crdsDeps() (*crds.Deps, error) {
	o := a.opts()
	cl, err := a.cluster()
	if err != nil {
		return nil, err
	}
	return &crds.Deps{
		Source:      a.NewCRDSource(o.KubeContext),
		Cluster:     cl,
		GitHub:      a.NewGitHub(),
		Prompt:      a.Prompt,
		P:           a.p,
		Out:         a.Out,
		Repo:        a.repo(),
		Env:         o.Env,
		Yes:         o.Yes || os.Getenv("CRDS_AUTO_CONFIRM") == "yes",
		Paged:       ui.IsTerminal(a.Out),
		Chart:       a.v.GetString("crds-chart"),
		Wait:        waitTimeout(a.v.GetString("crds-wait-timeout")),
		GitInfo:     gitinfo.Describe,
		PathPrefill: a.v.GetString("platform-chart-dir-prefill"),
	}, nil
}

// waitTimeout reads --wait-timeout / CRDS_WAIT_TIMEOUT. The bash command took bare seconds
// ("300"); a Go duration ("90s", "2m") works too. Unset or unparsable gives 0, so Deps uses its default.
func waitTimeout(s string) time.Duration {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second
	}
	d, _ := time.ParseDuration(s)
	return d
}

func (a *App) crdsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "crds",
		Short: "The platform-crds CRD bundle (menu: crds)",
		Long: `The GlueOps layer-0 CRD bundle (GlueOps/platform-crds): diff, confirm,
server-side apply.

Which CRDs a cluster installs is selected from ./platform.yaml (production
requires it; dev without one installs every profile). Run it before and after
the argocd step.

CRDs are only ever server-side applied (force-conflicts, field manager
glueops-platform-crds), never replaced: replace erases metadata.finalizers and
orphans the custom resources of a terminating CRD.`,
	}
	pf := c.PersistentFlags()
	pf.String("chart", "", "bundle chart reference override (default oci://ghcr.repo.gpkg.io/glueops/platform-crds); env: CRDS_CHART")
	pf.Duration("wait-timeout", crds.DefaultWait, "how long to wait for terminating CRDs to go and new ones to be Established; env: CRDS_WAIT_TIMEOUT")
	_ = a.v.BindPFlag("crds-chart", pf.Lookup("chart"))
	_ = a.v.BindPFlag("crds-wait-timeout", pf.Lookup("wait-timeout"))
	_ = a.v.BindEnv("crds-chart", "CRDS_CHART", "CAPTAIN_UTILS_CRDS_CHART")
	_ = a.v.BindEnv("crds-wait-timeout", "CRDS_WAIT_TIMEOUT", "CAPTAIN_UTILS_CRDS_WAIT_TIMEOUT")

	var o crds.Options
	var requireOwned bool
	add := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().StringVar(&o.Version, "version", "", "bundle release tag vX.Y.Z (default: the VERSIONS/glueops.yaml pin; dev apply: a chooser)")
		cmd.Flags().StringVar(&o.ChartDir, "chart-dir", "", "a local platform-crds checkout rendered by hack/render.sh (custom)")
		cmd.MarkFlagsMutuallyExclusive("version", "chart-dir")
		return cmd
	}
	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Read-only: is the cluster in sync with the bundle? (exit 0 in sync, 1 drift, 2 failure)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.crdsDeps()
			if err == nil {
				o.RequireOwned = requireOwned
				err = crds.Check(cmd.Context(), d, o)
			}
			switch {
			case err == nil:
				return nil
			case errors.Is(err, crds.ErrDrift):
				return &ExitError{Code: 1, Silent: true}
			}
			return &ExitError{Code: 2, Err: err}
		},
	}
	checkCmd.Flags().BoolVar(&requireOwned, "require-owned", false, "also exit 1 when a CRD is not yet owned by the glueops-platform-crds field manager (default: a note; apply moves ownership)")
	c.AddCommand(add(checkCmd), add(&cobra.Command{
		Use:   "apply",
		Short: "Diff, confirm, strip ArgoCD tracking, server-side apply, wait Established",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.crdsDeps()
			if err != nil {
				return err
			}
			return crds.Apply(cmd.Context(), d, o)
		},
	}))
	return c
}
