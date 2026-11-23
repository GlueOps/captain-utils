package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/glueops/captain-utils/internal/captain"
	"github.com/glueops/captain-utils/internal/ui"
)

// The interactive menu: what the codespace ran as `captain_utils`. Every item runs the
// subcommand of the same area through a fresh command tree, so the menu and the
// subcommands cannot drift apart. Menu order, item names and wording are the bash menu's.

const (
	itemExit      = "Exit"
	itemDiffTable = "show_diff_table"
	itemCRDs      = "crds"
	itemArgocd    = "argocd"
	itemPlatform  = "glueops-platform"
	itemPreflight = "app_chart_preflight"
	itemAWS       = "aws"
	itemPods      = "inspect_pods"
)

// menu holds the menu's seams, so tests script the prompts and the commands.
type menu struct {
	a    *App
	repo captain.Repo
	// run executes a subcommand for env and returns its exit code (errors are printed by it).
	run func(ctx context.Context, env string, args ...string) int
	// preflight runs the app-chart-preflight binary and returns its exit code.
	preflight func(ctx context.Context, bin string, args []string) int
	lookPath  func(string) (string, error)
	getenv    func(string) string
}

// menuCmd is `captain-utils menu`, also what `captain_utils` with no arguments runs.
func (a *App) menuCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "menu",
		Short: "The interactive menu (what captain_utils runs)",
		Long: `The interactive menu: dev or production, then crds, argocd, glueops-platform,
aws and the rest, as the codespace's captain_utils menu offered them. Each item
runs the subcommand of the same name; run from the captain repo root.

The environment is chosen in the menu, so --env and the environment variable
are ignored here. Esc or Ctrl-C at a menu leaves it; Ctrl-C while an item runs
stops that item and returns to the menu.`,
		Args: cobra.NoArgs,
		// the root's check of --env does not apply: the menu asks
		PersistentPreRunE: func(*cobra.Command, []string) error {
			a.p = ui.New(a.Out, a.Err, a.opts().NoColor)
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// a leftover CRDS_AUTO_CONFIRM must not skip the confirm the menu shows
			_ = os.Unsetenv("CRDS_AUTO_CONFIRM")
			m := a.newMenu()
			if code := m.Run(cmd.Context()); code != 0 {
				return &ExitError{Code: code, Silent: true}
			}
			return nil
		},
	}
}

func (a *App) newMenu() *menu {
	m := &menu{a: a, repo: a.repo(), lookPath: exec.LookPath, getenv: os.Getenv}
	m.run = m.runSubcommand
	m.preflight = m.execPreflight
	return m
}

// child is an App with the same dependencies and no command state, for running one
// subcommand while the menu's own command is still active.
func (a *App) child() *App {
	return &App{
		Version: a.Version, Commit: a.Commit, Out: a.Out, Err: a.Err,
		NewCluster: a.NewCluster, NewHelm: a.NewHelm, NewGitHub: a.NewGitHub,
		NewCRDSource: a.NewCRDSource, Prompt: a.Prompt,
	}
}

// itemContext is the context an item runs under: Ctrl-C cancels it and nothing else, so the
// menu survives. The menu's own context is not used for cancellation (see menuCmd).
func itemContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.WithoutCancel(ctx), os.Interrupt)
}

// globalArgs are the settings every item inherits from the menu's command line.
func (m *menu) globalArgs(env string) []string {
	o := m.a.opts()
	args := []string{"--env", env, "--captain-dir", o.CaptainDir}
	if o.KubeContext != "" {
		args = append(args, "--kube-context", o.KubeContext)
	}
	if o.Yes {
		args = append(args, "--yes")
	}
	if o.NoColor {
		args = append(args, "--no-color")
	}
	return args
}

func (m *menu) runSubcommand(ctx context.Context, env string, args ...string) int {
	ictx, stop := itemContext(ctx)
	defer stop()
	return m.a.child().Execute(ictx, append(m.globalArgs(env), args...))
}

// choose asks one question; Esc and Ctrl-C come back as ui.Back.
func (m *menu) choose(options ...string) (string, error) {
	return m.a.Prompt.Choose("", options...)
}

// Run is the menu loop; it returns the process exit code.
func (m *menu) Run(ctx context.Context) int {
	p := m.a.p
	if !m.a.Prompt.Interactive() {
		p.Error("the menu needs a terminal (run a subcommand instead, see captain-utils --help)")
		return 1
	}
	ok, err := m.checkCodespaceVersion()
	if err != nil {
		p.Error("%v", err)
		return 1
	}
	if !ok {
		return 1
	}
	env, err := m.choose("dev", "production", itemExit)
	if err != nil {
		p.Error("%v", err)
		return 1
	}
	switch env {
	case "dev", "production":
		if err := m.session(ctx, env); err != nil {
			p.Error("%v", err)
			return 1
		}
	}
	fmt.Fprintln(m.a.Out, "Goodbye!")
	return 0
}

// checkCodespaceVersion is check_codespace_version_match: the codespace image the
// captain repo asks for (codespace_version) against the one running (VERSION).
// It returns false when the operator declines to go on with a mismatch.
func (m *menu) checkCodespaceVersion() (bool, error) {
	pins, err := m.repo.GlueOpsVersions()
	if err != nil {
		m.a.p.Note("codespace version not checked: %v", err)
		return true, nil
	}
	want := pins.All(captain.CodespaceVersion)
	have := m.getenv("VERSION")
	if len(want) == 0 || have == "" {
		m.a.p.Note("codespace version not checked (needs a %s pin and $VERSION)", captain.CodespaceVersion)
		return true, nil
	}
	if want[0] == have {
		return true, nil
	}
	m.a.p.Alert("Current codespace version doesn't match with the desired: %s", want[0])
	return m.a.Prompt.Confirm("Confirmation")
}

// session is show_production / show_dev: the component loop, until Exit.
func (m *menu) session(ctx context.Context, env string) error {
	items := []string{itemCRDs, itemArgocd, itemPlatform, itemPreflight, itemAWS, itemExit}
	if env == "production" {
		items = append([]string{itemDiffTable}, items[:len(items)-1]...)
		items = append(items, itemPods, itemExit)
	}
	for {
		item, err := m.choose(items...)
		if err != nil {
			return err
		}
		switch item {
		case itemDiffTable:
			m.a.p.Highlight("Showing diff table before proceeding")
			m.run(ctx, env, "versions", "diff")
		case itemCRDs:
			m.crds(ctx, env)
		case itemArgocd:
			m.run(ctx, env, "argocd", "upgrade")
		case itemPlatform:
			m.run(ctx, env, "platform", "upgrade")
		case itemPreflight:
			m.appChartPreflight(ctx)
		case itemAWS:
			if bye, err := m.awsOptions(ctx, env); bye || err != nil {
				return err
			}
		case itemPods:
			m.a.p.Highlight("Inspecting pods in the cluster")
			m.run(ctx, env, "pods", "watch")
		default: // Exit, or Esc / Ctrl-C (gum choose ended the bash menu too)
			return nil
		}
	}
}

// crds is handle_crds. Run it before argocd and again after: the second run is a no-op
// unless the argocd release removed a CRD. It never ends the menu.
func (m *menu) crds(ctx context.Context, env string) {
	p := m.a.p
	if env != "production" {
		// dev: the release chooser inside the command already offers custom and Back
		if m.run(ctx, env, "crds", "apply") != 0 {
			p.Alert("platform-crds NOT applied")
		}
		return
	}
	if !m.repo.CRDsBundleEnabled() {
		p.Warn("This cluster is not on the platform-crds bundle yet: VERSIONS/glueops.yaml has no valid platform_crds_version pin.")
		p.Warn("ArgoCD's CRDs are still applied by the argocd step. Bump the terraform module and 'terraform apply' the captain repo to enable the bundle.")
		return
	}
	pins, err := m.repo.GlueOpsVersions()
	if err != nil {
		p.Alert("could not determine the platform-crds version")
		return
	}
	v := pins.All(captain.PlatformCRDsVersion)[0]
	choice, err := m.choose(v, "custom", ui.Back)
	if err != nil || choice == ui.Back {
		return
	}
	if choice != "custom" {
		if m.run(ctx, env, "crds", "apply", "--version", v) != 0 {
			p.Alert("platform-crds %s NOT applied", v)
		}
		return
	}
	// custom: apply the bundle from a local checkout of GlueOps/platform-crds (after hack/render.sh)
	p.Alert("⚠️  custom applies UNRELEASED CRDs to this cluster. Nothing records it afterwards (same field manager as the pinned bundle) — the next pinned run's diff shows what it changed.")
	fmt.Fprintln(p.Err, "Local platform-crds directory — a checkout of GlueOps/platform-crds with crds/ rendered (Tab completes, empty = back, Ctrl-C exits captain_utils):")
	dir, err := m.a.Prompt.Path("path>", m.a.v.GetString("platform-chart-dir-prefill"))
	if err != nil || dir == "" {
		return
	}
	if m.run(ctx, env, "crds", "apply", "--chart-dir", dir) != 0 {
		p.Alert("platform-crds from %s NOT applied", dir)
	}
}

// awsOptions is handle_aws_options. Exit here ends the whole menu, as it did in bash.
func (m *menu) awsOptions(ctx context.Context, env string) (bye bool, err error) {
	c, err := m.choose("calico", "eks-addons", "upgrade-eks-nodepools", "upgrade-kubernetes", itemExit)
	if err != nil {
		return false, err
	}
	switch c {
	case "calico":
		m.run(ctx, env, "aws", "calico", "upgrade")
	case "eks-addons":
		m.run(ctx, env, "aws", "eks-addons")
	case "upgrade-eks-nodepools":
		m.run(ctx, env, "aws", "nodepools")
	case "upgrade-kubernetes":
		m.run(ctx, env, "aws", "kubernetes")
	default:
		return true, nil
	}
	return false, nil
}

// appChartPreflight is handle_app_chart_preflight: read-only, run before bumping an
// ApplicationSet's app-chart targetRevision in manifests/manifest.yaml. It never ends the menu.
func (m *menu) appChartPreflight(ctx context.Context) {
	p := m.a.p
	bin := m.getenv("CAPTAIN_UTILS_PREFLIGHT")
	if bin == "" {
		bin = "app-chart-preflight"
	}
	if _, err := m.lookPath(bin); err != nil {
		p.Alert("app-chart-preflight is not installed in this codespace image")
		return
	}
	fmt.Fprintln(p.Err, "Target app chart revision: 0.18.0 for sets that source the chart from helm.gpkg.io or OCI, v0.18.0 or a branch for the chart's git repo (empty = back):")
	target, err := m.a.Prompt.Input("target>", "")
	if err != nil {
		return
	}
	target = strings.Join(strings.Fields(target), "")
	if target == "" {
		return
	}
	sets := m.applicationSets()
	choice, err := m.choose(append(append([]string{"all"}, sets...), ui.Back)...)
	if err != nil || choice == ui.Back {
		return
	}
	args := []string{"--target", target}
	if choice != "all" {
		args = append(args, "--appset", choice)
	}
	// the menu's other items use --kube-context; preflight must look at the same cluster
	if kc := m.a.opts().KubeContext; kc != "" {
		args = append(args, "--context", kc)
	}
	ictx, stop := itemContext(ctx)
	defer stop()
	switch rc := m.preflight(ictx, bin, args); rc {
	case 0:
		if choice == "all" {
			p.Plain("Preflight ok: bump only the sets under \"Safe to bump\" above to %s; sets under \"Not checked\" were not checked.", target)
		} else {
			p.Plain("Preflight ok: safe to bump %s to %s.", choice, target)
		}
	case 1:
		p.Alert("Preflight found changes or render errors: do NOT bump the sets it names until the values are fixed.")
	default:
		p.Warn("Preflight gave no verdict for some sets (exit %d): see \"No verdict\" above. A set that needs another target form (X.Y.Z for helm.gpkg.io or OCI, a tag or branch for git) gets none from a shared target, so pick that set on its own with its own target. A lost connection or timeout can be run again.", rc)
	}
}

// applicationSets lists the ApplicationSet names in manifests/manifest.yaml (none when it is absent).
func (m *menu) applicationSets() []string {
	f, err := os.Open(m.repo.Path("manifests", "manifest.yaml"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	dec := yaml.NewDecoder(f)
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&doc); err != nil {
			if !errors.Is(err, io.EOF) {
				m.a.p.Note("manifests/manifest.yaml: %v", err)
			}
			return out
		}
		if doc.Kind == "ApplicationSet" && doc.Metadata.Name != "" {
			out = append(out, doc.Metadata.Name)
		}
	}
}

// execPreflight runs app-chart-preflight (the one program captain-utils still executes:
// it is a separately released tool). Its output goes straight to the terminal.
func (m *menu) execPreflight(ctx context.Context, bin string, args []string) int {
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, m.a.Out, m.a.Err
	err := c.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	m.a.p.Error("run %s: %v", bin, err)
	return 127
}

// menuArgs is what the process runs for argv0 and its arguments: the codespace's
// `captain_utils` (a link to this binary) with no arguments opens the menu.
func menuArgs(argv0 string, args []string) []string {
	if len(args) == 0 && filepath.Base(argv0) == "captain_utils" {
		return []string{"menu"}
	}
	return args
}
