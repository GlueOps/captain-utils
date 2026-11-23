# AGENTS.md

`captain-utils`: the Go replacement of the codespace's `captain_utils` (bash menu
`.devcontainer/tools/captain_utils.sh`, `.devcontainer/libexec/captain_utils/{crds,custom.sh}`
and the Python `.devcontainer/tools/script_captain_utils` in GlueOps/codespaces). README has
the phase table.

## Layout

- `main.go`: sets `version`/`commit` (ldflags) and calls `internal/cmd`.
- `internal/cmd`: cobra tree + viper (flag > env > default). One file per area. `App.NewCluster`
  is the seam tests replace. `menu.go` is the interactive menu: every item runs a subcommand
  through a child `App`, so the menu cannot drift from the commands; `app_chart_preflight` is the
  one exec.
- `internal/captain`: reads the captain repo (VERSIONS pins, paths). Never writes.
- `internal/kube`: client-go reads; Helm releases are read from Helm's own Secrets.
- `internal/aws`: line-based main.tf edits (golden files in `testdata/`), tfstate reader.
- `internal/versions`: the current-vs-planned rows.
- `internal/ui`: output and prompts. Data to stdout, status to stderr; colour only on a terminal.
  `Prompter` (huh forms, bubbles pager) with `Script` for tests.
- `internal/helmx`: Helm SDK: repo update/add, version search, the helm-diff-equivalent diff,
  `upgrade --install`.
- `internal/upgrade`: the platform / argocd / calico flows, behind interfaces (`Helm`, `Kube`,
  `Releases`) so tests script them; tests are named after the bash scenarios they replace.
- `internal/crds`: the platform-crds bundle flows (`Apply`, `Check`) behind `Source`, `Cluster` and
  `Releases` interfaces. `doApply` is the only mutating Apply and needs a `clearance` from `clear`;
  a test enforces that and that nothing replaces/updates a CRD.
- `internal/gitinfo` (go-git), `internal/github` (release tags over REST).

## Rules

- No exec of helm, kubectl, gh, gum, yq, jq or python. The one exception is
  `app-chart-preflight` (a separately released tool), run by the menu.
- `helmx.Diff` must keep printing what `helm diff upgrade --allow-unreleased` prints: compare
  against the plugin on a real captain after touching it.
- Behaviour is ported as it was, prompt wording and menu order included; any deliberate change
  is listed in README "Changes from ..." and in the commit message.
- Commands that change the cluster ask first; without a terminal they need `--yes`. Edits to
  files in the captain repo print a diff and support `--dry-run`.
- VERSIONS files are read with yaml.v3 into strings so `1.30` stays `1.30`.
- CI: gofmt, `go mod tidy` diff, vet, build, `go test -race`. Actions pinned by SHA.
- Conventional Commits, release-please friendly.
