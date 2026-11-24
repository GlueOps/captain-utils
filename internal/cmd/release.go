package cmd

import (
	"context"
	"fmt"
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
	add := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().StringVar(&o.Version, "version", "", "chart version (required without a terminal; at a terminal a chooser offers the pin)")
		cmd.Flags().StringVar(&o.ChartDir, "chart-dir", "", "install from this local chart directory (custom)")
		cmd.MarkFlagsMutuallyExclusive("version", "chart-dir")
		return cmd
	}
	c.AddCommand(add(&cobra.Command{
		Use:   "diff",
		Short: "Print the helm diff, change nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.DiffOnly = true
			d, err := a.deps(true)
			if err != nil {
				return err
			}
			return upgrade.Platform(cmd.Context(), d, o, a.Out)
		},
	}), add(&cobra.Command{
		Use:   "upgrade",
		Short: "Diff, confirm, helm upgrade --install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.deps(false)
			if err != nil {
				return err
			}
			return upgrade.Platform(cmd.Context(), d, o, a.Out)
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
	add := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().StringVar(&o.Version, "version", "", "argo-cd chart version (required without a terminal; at a terminal a chooser offers the pin)")
		return cmd
	}
	c.AddCommand(add(&cobra.Command{
		Use:   "diff",
		Short: "Print the helm diff, change nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.DiffOnly = true
			d, err := a.deps(true)
			if err != nil {
				return err
			}
			return upgrade.Argocd(cmd.Context(), d, o, a.Out)
		},
	}), add(&cobra.Command{
		Use:   "upgrade",
		Short: "Diff, confirm, helm upgrade --install",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.deps(false)
			if err != nil {
				return err
			}
			return upgrade.Argocd(cmd.Context(), d, o, a.Out)
		},
	}))
	return c
}

func (a *App) calicoCmd() *cobra.Command {
	c := &cobra.Command{Use: "calico", Short: "Calico on EKS (menu: aws → calico)"}
	c.AddCommand(&cobra.Command{
		Use:   "upgrade",
		Short: "Diff, confirm, delete the aws-node daemonset and helm upgrade --install calico at the pin",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.deps(false)
			if err != nil {
				return err
			}
			return upgrade.Calico(cmd.Context(), d, a.Out)
		},
	})
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
			"\n(as `watch kubectl get pods -A | grep ...` did), refreshed every --interval.",
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
	c.AddCommand(watch)
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
