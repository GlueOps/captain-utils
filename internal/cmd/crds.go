package cmd

import (
	"errors"
	"fmt"
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
		Prompt:      a.Prompt,
		P:           a.p,
		Out:         a.Out,
		Repo:        a.repo(),
		Yes:         o.Yes,
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

Which CRDs a cluster installs is selected from ./platform.yaml (required). Run it
before and after the argocd step.

Without a terminal, --version or --chart-dir is required; at a terminal, apply asks
(the VERSIONS/glueops.yaml pin, custom, or Back) and check uses the pin.

Exit codes: 0 done or nothing to do, 1 failed (apply: including partial; check:
drift), 2 usage error (check: any failure), 3 apply declined.

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
	var checkOutput, output string
	add := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().StringVar(&o.Version, "version", "", "bundle release tag vX.Y.Z (required without a terminal; at a terminal apply offers the pin in a chooser, check uses it)")
		cmd.Flags().StringVar(&o.ChartDir, "chart-dir", "", "a local platform-crds checkout rendered by hack/render.sh (custom)")
		cmd.MarkFlagsMutuallyExclusive("version", "chart-dir")
		return cmd
	}
	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Read-only: is the cluster in sync with the bundle? (exit 0 in sync, 1 drift, 2 failure or usage error)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkOutput != "text" && checkOutput != "json" {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("--output must be text or json, got %q", checkOutput)}
			}
			d, err := a.crdsDeps()
			if err == nil && checkOutput == "json" {
				// stdout is only the JSON: the plan lines go to stderr with the other status
				d.Out = a.Err
			}
			var rep *crds.CheckReport
			if err == nil {
				o.RequireOwned = requireOwned
				rep, err = crds.CheckResult(cmd.Context(), d, o)
			}
			if checkOutput == "json" {
				if rep == nil {
					rep = crds.NewCheckReport(o.Version, 0)
					if err != nil {
						rep.Error = err.Error()
					}
				}
				if jerr := rep.WriteJSON(a.Out); jerr != nil && err == nil {
					err = jerr
				}
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
	applyCmd := add(&cobra.Command{
		Use:   "apply",
		Short: "Diff, confirm, strip ArgoCD tracking, server-side apply, wait Established",
		Long: `Diff, confirm, strip ArgoCD tracking, server-side apply, wait Established.

It ends with one result on stdout: "applied=N unchanged=N failed=N" (plus a line per
failed CRD), or with -o json {version, applied, unchanged, failed: [{name, error}],
established}. With -o json the plan and diff go to stderr, so stdout is only the JSON.
A declined confirm exits 3; a partial failure exits 1 with the failed CRDs in the result.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output != "text" && output != "json" {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("--output must be text or json, got %q", output)}
			}
			d, err := a.crdsDeps()
			var res *crds.Result
			if err == nil {
				if output == "json" {
					d.Out = a.Err
					d.Paged = false
				}
				res, err = crds.ApplyResult(cmd.Context(), d, o)
			}
			if output == "json" {
				// always one object on stdout, also for a usage error or a failure before any CRD
				if res == nil && err != nil {
					res = crds.NewResult(o.Version)
				}
				if res != nil {
					if err != nil {
						res.Error = err.Error()
					}
					if jerr := res.WriteJSON(a.Out); jerr != nil && err == nil {
						err = jerr
					}
				}
			} else if res != nil && !errors.Is(err, ui.ErrUsage) {
				res.WriteText(a.Out)
			}
			return err
		},
	})
	applyCmd.Flags().StringVarP(&output, "output", "o", "text", "result format: text or json")
	checkCmd.Flags().StringVarP(&checkOutput, "output", "o", "text", "result format: text or json ({version, inSync, total, new, changed, differs, terminating, notEstablished, notOwned, notOnCluster, reasons}; stdout is only the JSON)")
	c.AddCommand(add(checkCmd), applyCmd)
	return c
}
