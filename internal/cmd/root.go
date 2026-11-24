// Package cmd is the captain_utils command line: the cobra tree, flag/env
// binding through viper, and the exit codes.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/client-go/rest"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/crds"
	"github.com/glueops/captain-utils/internal/helmx"
	"github.com/glueops/captain-utils/internal/kube"
	"github.com/glueops/captain-utils/internal/ui"
	"github.com/glueops/captain-utils/internal/upgrade"
	"github.com/glueops/captain-utils/internal/versions"
)

// Exit codes. 0 is done (or nothing to do) and 1 a failure; crds check uses 1 for drift.
const (
	ExitUsage    = 2 // a command line that cannot run
	ExitDeclined = 3 // a confirmation was answered no: nothing was changed
)

// ExitError carries a specific exit code. Its message, if any, has already
// been printed when Silent is set.
type ExitError struct {
	Code   int
	Err    error
	Silent bool
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// Cluster is everything the commands read from or do to the cluster.
type Cluster interface {
	versions.Cluster
	upgrade.Kube
	crds.Cluster
	Pods(ctx context.Context, now time.Time) ([]kube.PodRow, error)
}

// App holds what every command shares. Tests replace the factories and Prompt.
type App struct {
	Version, Commit string
	Out, Err        io.Writer
	NewCluster      func(kubeContext string) (Cluster, error)
	NewHelm         func(kubeContext string) upgrade.Helm
	NewCRDSource    func(kubeContext string) crds.Source
	Prompt          ui.Prompter

	v *viper.Viper
	p *ui.Printer
	// yes is --yes, read from the flag alone: an exported environment variable outlives the
	// command it was set for, so only the explicit flag approves a confirmation.
	yes bool
	// running is set once a command's RunE has started: an error before that is a usage error.
	running bool
}

// Options are the resolved global settings (flag > env > default).
type Options struct {
	CaptainDir  string
	KubeContext string
	Yes         bool
	NoColor     bool
}

func (a *App) opts() Options {
	return Options{
		CaptainDir:  a.v.GetString("captain-dir"),
		KubeContext: a.v.GetString("kube-context"),
		Yes:         a.yes,
		NoColor:     a.v.GetBool("no-color"),
	}
}

func (a *App) repo() captain.Repo { return captain.Repo{Dir: a.opts().CaptainDir} }

func (a *App) cluster() (Cluster, error) {
	c, err := a.NewCluster(a.opts().KubeContext)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return c, nil
}

// NewApp wires the real dependencies.
func NewApp(version, commit string, out, errw io.Writer) *App {
	a := &App{Version: version, Commit: commit, Out: out, Err: errw}
	a.NewCluster = func(kubeContext string) (Cluster, error) {
		return kube.New(kubeContext, "captain-utils/"+version)
	}
	a.NewHelm = func(kubeContext string) upgrade.Helm { return helmx.New(kubeContext, errw) }
	a.NewCRDSource = func(kubeContext string) crds.Source { return helmx.New(kubeContext, errw) }
	a.Prompt = ui.NewTTY()
	return a
}

// Root builds the command tree.
func (a *App) Root() *cobra.Command {
	a.v = viper.New()
	root := &cobra.Command{
		Use:   "captain-utils",
		Short: "Upgrade and inspect a GlueOps captain from its captain repo",
		Long: `captain-utils upgrades and inspects a GlueOps captain (cluster) from the root of
its captain repo, using your current kube context.

It is the Go replacement of the codespace's captain_utils menu. Every menu item
is a subcommand with flags, and "captain-utils menu" (or this binary run as
captain_utils with no arguments) is the menu itself.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       a.Version,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			a.p = ui.New(a.Out, a.Err, a.opts().NoColor)
			return nil
		},
	}
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	root.SetVersionTemplate(fmt.Sprintf("captain-utils %s (%s)\n", a.Version, a.Commit))

	pf := root.PersistentFlags()
	pf.String("captain-dir", ".", "captain repo root; env: CAPTAIN_UTILS_CAPTAIN_DIR")
	pf.String("kube-context", "", "kube context (default: the current context); env: CAPTAIN_UTILS_KUBE_CONTEXT")
	pf.BoolVarP(&a.yes, "yes", "y", false, "answer yes to confirmations (needed to change the cluster without a terminal); no environment variable sets it")
	pf.Bool("no-color", false, "no colour (also off when output is not a terminal, or NO_COLOR is set)")
	for _, name := range []string{"captain-dir", "kube-context", "no-color"} {
		_ = a.v.BindPFlag(name, pf.Lookup(name))
	}
	a.v.SetEnvPrefix("CAPTAIN_UTILS")
	a.v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	a.v.AutomaticEnv()
	// where the "custom" directory prompt starts, as in the bash menu
	a.v.SetDefault("platform-chart-dir-prefill", "/workspaces/")
	_ = a.v.BindEnv("platform-chart-dir-prefill", "PLATFORM_CHART_DIR_PREFILL")

	root.AddCommand(a.versionCmd(), a.versionsCmd(), a.awsCmd(), a.platformCmd(), a.argocdCmd(), a.crdsCmd(), a.podsCmd(), a.menuCmd())
	rejectUnknownSubcommands(root)
	// every error cobra raises before a command runs (unknown flag or command, a bad flag value,
	// exclusive flags, wrong arguments) is a usage error: exit 2, never a command's own code
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &ExitError{Code: ExitUsage, Err: err} })
	a.markRunning(root)
	return root
}

func (a *App) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintf(a.Out, "captain-utils %s (%s)\n", a.Version, a.Commit)
			return nil
		},
	}
}

// Execute runs the CLI and returns the process exit code.
func (a *App) Execute(ctx context.Context, args []string) int {
	root := a.Root()
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		a.printer().Warn("interrupted")
		return 130
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		if !ee.Silent && ee.Err != nil {
			a.printer().Error("%v", ee.Err)
		}
		return ee.Code
	}
	if errors.Is(err, ui.ErrDeclined) {
		return ExitDeclined
	}
	a.printer().Error("%v", err)
	if errors.Is(err, ui.ErrUsage) || !a.running {
		return ExitUsage
	}
	return 1
}

// markRunning wraps every command's RunE to record that the command line was accepted.
func (a *App) markRunning(c *cobra.Command) {
	if run := c.RunE; run != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			a.running = true
			return run(cmd, args)
		}
	}
	for _, sub := range c.Commands() {
		a.markRunning(sub)
	}
}

// printer is usable even when PersistentPreRunE did not run (flag errors).
func (a *App) printer() *ui.Printer {
	if a.p == nil {
		a.p = ui.New(a.Out, a.Err, a.v != nil && a.v.GetBool("no-color"))
	}
	return a.p
}

// Main is main.main. Ctrl-C or SIGTERM cancels the running command's context.
func Main(version, commit string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// API server warnings once each, as "Warning: ..." (as kubectl and helm print them), not as klog lines
	rest.SetDefaultWarningHandler(rest.NewWarningWriter(os.Stderr, rest.WarningWriterOptions{Deduplicate: true}))
	return NewApp(version, commit, os.Stdout, os.Stderr).Execute(ctx, menuArgs(os.Args[0], os.Args[1:]))
}

// rejectUnknownSubcommands makes a group command (one that only has subcommands)
// fail on an argument that is not one of them. Cobra returns a group's help and
// exit 0 before it ever validates arguments, so a script calling a subcommand that
// no longer exists (the bash crds command's apply-dir, say) would silently do
// nothing. A group on its own still prints its help.
func rejectUnknownSubcommands(c *cobra.Command) {
	for _, sub := range c.Commands() {
		if sub.HasSubCommands() && sub.Run == nil && sub.RunE == nil {
			sub.RunE = func(cmd *cobra.Command, args []string) error {
				if len(args) > 0 {
					return &ExitError{Code: ExitUsage, Err: fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())}
				}
				return cmd.Help()
			}
		}
		rejectUnknownSubcommands(sub)
	}
}
