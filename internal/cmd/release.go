package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/glueops/captain-utils/internal/gitinfo"
	"github.com/glueops/captain-utils/internal/kube"
	"github.com/glueops/captain-utils/internal/ui"
	"github.com/glueops/captain-utils/internal/upgrade"
)

// deps builds the flows' dependencies. diffOnly sends the diff to stdout
// instead of the pager, which decides where colour is judged.
func (a *App) deps(diffOnly bool) (*upgrade.Deps, error) {
	o := a.opts()
	cl, err := a.cluster()
	if err != nil {
		return nil, err
	}
	colorOn := !o.NoColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	if diffOnly {
		colorOn = colorOn && ui.IsTerminal(a.Out)
	} else {
		colorOn = colorOn && ui.IsTerminal(a.Err)
	}
	prefill := a.v.GetString("platform-chart-dir-prefill")
	return &upgrade.Deps{
		Helm:        a.NewHelm(o.KubeContext),
		Kube:        cl,
		Prompt:      a.Prompt,
		P:           a.p,
		Repo:        a.repo(),
		Yes:         o.Yes,
		Color:       colorOn,
		GitInfo:     gitinfo.Describe,
		PathPrefill: prefill,
	}, nil
}

// runFlow runs one upgrade flow with the -o text|json choice. With json, stdout is only the
// result object: the flow's own text and status go to stderr, the diff goes into the result
// (unless noDiff), and a failure, a usage error or a declined confirm is also written there
// (with "error"), besides the exit code and the message on stderr.
func (a *App) runFlow(output string, noDiff, diffOnly bool, flow func(d *upgrade.Deps, out io.Writer) error) error {
	if err := checkFormat(output); err != nil {
		return err
	}
	if noDiff && output != "json" {
		return &ExitError{Code: ExitUsage, Err: fmt.Errorf("--no-diff only applies to -o json")}
	}
	d, err := a.deps(diffOnly)
	if output == "text" {
		if err != nil {
			return err
		}
		return flow(d, a.Out)
	}
	res := upgrade.NewResult()
	if err == nil {
		d.Color = false
		d.Result, d.NoDiff = res, noDiff
		err = flow(d, a.Err)
	}
	switch {
	case errors.Is(err, ui.ErrDeclined):
		res.Action = upgrade.ActionDeclined
		res.Error = err.Error()
	case err != nil:
		res.Error = err.Error()
	}
	if jerr := res.WriteJSON(a.Out); jerr != nil && err == nil {
		err = jerr
	}
	return err
}

func addOutputFlag(c *cobra.Command, output *string, noDiff *bool) *cobra.Command {
	c.Flags().StringVarP(output, "output", "o", "text", "result format: text or json (stdout is then only the JSON: {release, namespace, chart, version, action, changed, diff, diffLines, status, revision, notes, error})")
	c.Flags().BoolVar(noDiff, "no-diff", false, "with -o json: leave the helm diff text out of the result (changed and diffLines still say whether and how much it changed)")
	return c
}

func (a *App) platformCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "platform",
		Short: "The glueops-platform release (menu: glueops-platform)",
		Long: `The glueops-platform release (menu item glueops-platform), in glueops-core,
with -f platform.yaml -f overrides.yaml (platform.yaml twice when there is no
overrides.yaml).

At a terminal a chooser offers the VERSIONS/glueops.yaml pin (or custom, or Back);
without one, --version or --chart-dir is required. --chart-dir installs a local
checkout of GlueOps/platform-helm-chart-platform instead ("custom"), recorded in the
release description.`,
	}
	var o upgrade.PlatformOptions
	var output string
	var noDiff bool
	add := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().StringVar(&o.Version, "version", "", "chart version (required without a terminal; at a terminal a chooser offers the pin)")
		cmd.Flags().StringVar(&o.ChartDir, "chart-dir", "", "install from this local chart directory (custom)")
		cmd.MarkFlagsMutuallyExclusive("version", "chart-dir")
		return addOutputFlag(cmd, &output, &noDiff)
	}
	c.AddCommand(add(&cobra.Command{
		Use:   "diff",
		Short: "Print the helm diff, change nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.DiffOnly = true
			return a.runFlow(output, noDiff, true, func(d *upgrade.Deps, out io.Writer) error {
				return upgrade.Platform(cmd.Context(), d, o, out)
			})
		},
	}), add(&cobra.Command{
		Use:   "upgrade",
		Short: "Diff, confirm, helm upgrade --install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runFlow(output, noDiff, false, func(d *upgrade.Deps, out io.Writer) error {
				return upgrade.Platform(cmd.Context(), d, o, out)
			})
		},
	}))
	return c
}

func (a *App) argocdCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "argocd",
		Short: "The argocd release (menu: argocd)",
		Long: `The argocd release (menu item argocd): argo/argo-cd in glueops-core with -f
argocd.yaml and --skip-crds.

ArgoCD's CRDs come from the platform-crds bundle, not from this step: run crds
before and after it. Without a terminal, --version is required.`,
	}
	var o upgrade.ArgocdOptions
	var output string
	var noDiff bool
	add := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().StringVar(&o.Version, "version", "", "argo-cd chart version (required without a terminal; at a terminal a chooser offers the pin)")
		return addOutputFlag(cmd, &output, &noDiff)
	}
	c.AddCommand(add(&cobra.Command{
		Use:   "diff",
		Short: "Print the helm diff, change nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.DiffOnly = true
			return a.runFlow(output, noDiff, true, func(d *upgrade.Deps, out io.Writer) error {
				return upgrade.Argocd(cmd.Context(), d, o, out)
			})
		},
	}), add(&cobra.Command{
		Use:   "upgrade",
		Short: "Diff, confirm, helm upgrade --install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runFlow(output, noDiff, false, func(d *upgrade.Deps, out io.Writer) error {
				return upgrade.Argocd(cmd.Context(), d, o, out)
			})
		},
	}))
	return c
}

func (a *App) calicoCmd() *cobra.Command {
	c := &cobra.Command{Use: "calico", Short: "Calico on EKS (menu: aws → calico)"}
	var output string
	var noDiff bool
	c.AddCommand(addOutputFlag(&cobra.Command{
		Use:   "upgrade",
		Short: "Diff, confirm, delete the aws-node daemonset and helm upgrade --install calico at the pin",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runFlow(output, noDiff, false, func(d *upgrade.Deps, out io.Writer) error {
				return upgrade.Calico(cmd.Context(), d, out)
			})
		},
	}, &output, &noDiff))
	return c
}

func (a *App) podsCmd() *cobra.Command {
	c := &cobra.Command{Use: "pods", Short: "Pods across the cluster (menu: inspect_pods)"}
	var interval time.Duration
	var once, all bool
	watch := &cobra.Command{
		Use:   "watch",
		Short: "Refresh the list of unhealthy pods until Ctrl-C",
		Long: "Lists every pod whose status contains " + strings.Join(kube.UnhealthyStatuses, ", ") +
			"\n(as `watch kubectl get pods -A | grep ...` did), refreshed every --interval.\nIt never returns on its own: scripts use --once or `pods list` for one snapshot.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := a.cluster()
			if err != nil {
				return err
			}
			return a.watchPods(cmd.Context(), cl, interval, once, all)
		},
	}
	watch.Flags().DurationVar(&interval, "interval", 5*time.Second, "refresh interval")
	watch.Flags().BoolVar(&once, "once", false, "print one snapshot and exit")
	watch.Flags().BoolVar(&all, "all", false, "list every pod, not only unhealthy ones")
	var output string
	var listAll bool
	list := &cobra.Command{
		Use:   "list",
		Short: "One snapshot of the unhealthy pods (every pod with --all), then exit",
		Long: "Prints once, without a refresh loop, the pods whose status contains " + strings.Join(kube.UnhealthyStatuses, ", ") +
			".\nWith -o json stdout is a JSON array of {namespace, name, ready, status, restarts, age} ([] when none).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkFormat(output); err != nil {
				return err
			}
			cl, err := a.cluster()
			if err != nil {
				if output == "json" {
					a.writeJSONError(err)
				}
				return err
			}
			if output == "text" {
				return a.watchPods(cmd.Context(), cl, 0, true, listAll)
			}
			rows, err := cl.Pods(cmd.Context(), time.Now())
			if err != nil {
				a.writeJSONError(err)
				return err
			}
			out := []kube.PodRow{}
			for _, r := range rows {
				if listAll || kube.Unhealthy(r) {
					out = append(out, r)
				}
			}
			enc := json.NewEncoder(a.Out)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		},
	}
	list.Flags().StringVarP(&output, "output", "o", "text", "result format: text or json")
	list.Flags().BoolVar(&listAll, "all", false, "list every pod, not only unhealthy ones")
	c.AddCommand(watch, list)
	return c
}

func (a *App) watchPods(ctx context.Context, cl Cluster, interval time.Duration, once, all bool) error {
	clear := ui.IsTerminal(a.Out) && !once
	for {
		now := time.Now()
		rows, err := cl.Pods(ctx, now)
		if ctx.Err() != nil {
			return nil
		}
		if clear {
			fmt.Fprint(a.Out, "\033[H\033[2J")
		}
		if !once {
			fmt.Fprintf(a.Out, "Every %s: pods %s    %s\n\n", interval, strings.Join(kube.UnhealthyStatuses, "|"), now.Format(time.RFC1123))
		}
		if err != nil {
			if once {
				return err // a snapshot's failure is stderr's (Execute prints it), not stdout's
			}
			fmt.Fprintf(a.Out, "%v\n", err)
		} else {
			fmt.Fprintln(a.Out, kube.PodHeader)
			for _, r := range rows {
				if all || kube.Unhealthy(r) {
					fmt.Fprintln(a.Out, r.String())
				}
			}
		}
		if once {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
