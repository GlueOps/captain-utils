# captain-utils

Upgrade and inspect a GlueOps captain (cluster) from the root of its captain
repo, with your current kube context.

`captain-utils` is the Go replacement of the codespace's `captain_utils`: a
bash menu, a bash `crds` command and a Python helper that called gum, yq, jq,
helm, kubectl, gh and python. This is one static binary with no tool
dependencies, and every menu item is also a subcommand with flags.

The port lands one area at a time. Until it is complete, the codespace ships
this binary as `captain-utils` next to the bash `captain_utils` menu, and the
menu calls it for each area that has been ported.

| Phase | Area | Status |
|---|---|---|
| 0 | CLI skeleton, captain repo reader, CI, release binaries | done |
| 1 | `versions diff` (show_diff_table), `aws eks-addons / nodepools / kubernetes` (replaces `script_captain_utils`) | done |
| 2 | `platform`, `argocd`, `aws calico upgrade`, `pods watch` (Helm SDK, helm-diff's own diff rendering) | done |
| 3 | `crds` (platform-crds bundle: `check`, `apply`; server-side apply) | done |
| 4 | the interactive menu, installed as `captain_utils` (`captain-utils menu`), `app_chart_preflight` | done |

## Usage

Run from the captain repo root (or pass `--captain-dir`).

```sh
captain-utils menu                          # the interactive menu: opens on the item list
captain_utils                               # the same: this binary installed under the old name (see below)
```

The menu offers what the bash menu did, in the same order: `show_diff_table`,
`crds`, `argocd`, `glueops-platform`, `app_chart_preflight`, `aws`, `inspect_pods`,
`Exit`. Each item runs the subcommand of the same area, so `crds` in the menu is `crds apply`.
Install this binary as `captain_utils` (`ln -s captain-utils /usr/local/bin/captain_utils`): run
under that name with no arguments it opens the menu; with arguments it behaves as `captain-utils`.

```sh
captain-utils versions diff                 # current vs planned table
captain-utils versions diff -o csv          # or -o json

captain-utils aws eks-addons --dry-run      # print the main.tf diff only
captain-utils aws eks-addons                # write terraform/kubernetes/main.tf
captain-utils aws kubernetes                # eks_version -> VERSIONS/aws.yaml kubernetes_version
captain-utils aws nodepools                 # copy every node pool at the new AMI, name -N -> -N+1
captain-utils aws nodepools --kubernetes-version 1.33   # without reading the cluster
```

```sh
captain-utils platform diff --version v0.85.0   # helm diff of that glueops-platform version
captain-utils platform upgrade              # at a terminal: a chooser offers the pin; diff (pager), confirm, helm upgrade --install
captain-utils platform upgrade --chart-dir ~/platform-helm-chart-platform   # "custom": a local chart
captain-utils argocd upgrade --version 9.3.7 --yes
captain-utils aws calico upgrade
captain-utils pods watch                    # unhealthy pods, every 5s; --once, --all
```

```sh
captain-utils crds check --version v0.1.6   # read-only; exit 0 in sync, 1 drift, 2 failure
captain-utils crds check --version v0.1.6 -o json   # the findings as JSON on stdout (plan on stderr)
captain-utils crds apply --version v0.1.6   # diff, confirm, apply; ends with applied=N unchanged=N failed=N
captain-utils crds apply --version v0.1.6 --yes -o json   # the result as JSON on stdout (plan and diff on stderr)
captain-utils crds apply --chart-dir ~/platform-crds   # "custom": a checkout rendered by hack/render.sh
```

`crds` reads `./platform.yaml` to pick the CRD profiles this cluster installs
(required). Run it before and after the `argocd` step: ArgoCD's CRDs come from the
bundle, and the `argocd` step applies none, so the order is `crds` → `argocd` → `crds`. CRDs are only ever server-side applied
(force-conflicts, field manager `glueops-platform-crds`), never replaced:
replace erases `metadata.finalizers` and orphans the custom resources of a
terminating CRD. `apply` always applies after a yes, also when nothing differs,
so field ownership lands on the manager; a CRD that is being deleted is waited
out first (`--wait-timeout`, default 120s), and a stored API version the bundle
no longer defines stops the run before the diff is shown. `--chart` (env
`CRDS_CHART`) overrides the bundle chart reference.

`diff` prints the same text `helm diff upgrade --allow-unreleased` prints
(helm-diff's own packages render it) and changes nothing. `upgrade` pages the
diff, asks, then runs the equivalent of the `helm upgrade --install` line it
prints. Without a terminal, `platform`, `argocd` and `crds` need an explicit `--version` (or
`--chart-dir`), and a change needs `--yes`. At a terminal a chooser offers the `VERSIONS/glueops.yaml`
pin (plus `custom` and `Back` where they exist).

### Exit codes

| Code | Meaning |
|---|---|
| 0 | done, or nothing to do |
| 1 | failed (including a partial `crds apply`); for `crds check`, drift |
| 2 | usage error: unknown flag, subcommand or flag value (an unusable `--version` or `--wait-timeout` included), exclusive flags, a missing `--version` or a missing `--yes` without a terminal (for `crds check` any failure is also 2) |
| 3 | a confirmation was declined: nothing was changed (`crds apply`, `platform upgrade`, `argocd upgrade`, `aws calico upgrade`) |
| 130 | interrupted |

`crds apply` ends with one result on stdout, `applied=N unchanged=N failed=N` plus a line per failed
CRD, or with `-o json`: `{"version", "applied": [...], "unchanged": [...], "failed": [{"name", "error"}],
"established": true|false, "error"}`. `applied` are CRDs applied with a content change, `unchanged` ones with none,
`established` is true once the post-apply wait saw every bundle CRD Established. A declined or in-sync run
applies nothing; `error` is set whenever the run did not finish (a failure, a usage error, a declined confirm), and
`applied` still lists what was applied when only the post-apply check failed. `error` is always there (`""` when
the run finished), and `appliedCount` / `unchangedCount` count the lists; `--summary` (with `-o json`, exit 2 otherwise)
leaves the CRD name lists out and keeps the counts and `failed`. The menu treats 3 as skipped, not as a failure.

`-o json` is also on `crds check`, `platform diff|upgrade`, `argocd diff|upgrade` and `aws calico upgrade`.
Stdout is then one JSON object and nothing else (the plan, status and helm text go to stderr); exit
codes are unchanged. A failure or a declined confirm still prints the object, with `error` set, so a
script reads one stream. That includes a usage error from the flow (for example a missing `--version`
without a terminal): exit 2, the object with `error` set and no findings. Only a command line cobra
rejects before the command runs prints `{"error": "..."}` when `-o json` is on the line (an unknown `-o` value such as `-o yaml` prints no JSON).
Lists are always `[]`, never `null`.

- `crds check`: `{"version", "inSync", "total", "new", "changed", "differs": [...], "terminating": [...],
  "notEstablished": [...], "notOwned": [...], "notOnCluster", "reasons": [...], "error"}` (`error` is `""` when the check ran; `--summary` leaves `differs` out). `inSync` is true
  exactly when the exit code is 0. When the check itself failed, `error` is set and the lists are empty.
- upgrades: `{"release", "namespace", "chart", "version", "action", "changed", "diff", "diffLines", "status",
  "revision", "notes", "warnings", "error"}`; every key is always there (`changed` is `null` when the run stopped before the diff,
  `warnings` is `[]` unless something went wrong without stopping the run, such as a failed `aws-node` delete). `action` is `diff` (a diff command), `installed` or `upgraded`, `declined` (exit 3) or
  `failed` (the upgrade was started and failed: check the release, and for calico `aws-node` is already removed) or
  `none` (nothing changed: stopped before the upgrade, see `error`). `changed` and `diff` are the helm diff, which -o json keeps
  out of the pager; `diff` is empty when nothing changed. At a terminal without `--yes` the diff is also shown on stderr before the confirm. Run `diff -o json`, then `upgrade --yes -o json`.
  `--no-diff` (with `-o json` only; exit 2 otherwise) leaves the diff text out, which for the platform chart is
  about 160 KB: `changed` and `diffLines` still say whether and how much changed.

The `aws` main.tf commands edit `terraform/kubernetes/main.tf` and nothing else: review
the diff, commit, and run tofu as usual. They also accept the old menu
names (`upgrade-kubernetes`, `upgrade-eks-nodepools`).

### Global flags

| Flag | Env | Default |
|---|---|---|
| `--captain-dir DIR` | `CAPTAIN_UTILS_CAPTAIN_DIR` | `.` |
| `--kube-context NAME` | `CAPTAIN_UTILS_KUBE_CONTEXT` | current context |
| `-y, --yes` | none: only the flag approves | off |
| `--no-color` | `NO_COLOR` | off |

A flag beats its environment variable. `--yes` has no environment variable: one exported for a
scripted run would approve every later command. Tables and diffs go to stdout, status
lines to stderr.

## Changes from the Python helper

- `versions diff` no longer rewrites `main.tf`, and works on captains without
  `terraform/kubernetes/main.tf` or `VERSIONS/aws.yaml` (k3d): those rows are
  left out with a note. A cluster read that fails shows `N/A` instead of
  crashing.
- `calico_ctl_version` shows `N/A*`: the helper showed the calico chart version,
  and nothing on the cluster reports calicoctl's.
- `aws eks-addons` sets the pins even without a local `terraform.tfstate` (the
  helper silently did nothing with a remote backend).
- `aws nodepools` accepts a blank line between the node pool list and
  `peering_configs` (the helper copied the `]` and broke the file), only
  changes the pool number in a copied name (the helper replaced every
  occurrence of the digit on the line), and fails with a message instead of a
  traceback when the cluster or a pin is missing.
- Empty values (`coredns_version = ""`) are set, not mangled.

## Changes from the bash menu (phase 2)

- No helm, helm-diff, kubectl, gh, gum, yq or jq: the Helm SDK (same settings,
  repositories.yaml and cache as the CLI) and client-go.
  Verified on a k3d captain: `platform diff` and `argocd diff` are
  byte-identical to `helm diff upgrade` (a 3,378-line argo-cd diff included).
- Only the repo a flow uses is refreshed (the menu ran `helm repo update` for
  every repo at startup).
- The legacy ArgoCD CRD install is removed (see "Changes from the bash menu (phase 4)").
- `aws calico upgrade` shows the helm diff of the calico release, then asks once before it
  deletes the `aws-node` daemonset and installs. A first install (no `calico` release yet) has no
  diff to show: validating the render needs the chart's own CRDs, which only the install creates, so the
  diff is skipped with a note and the confirm says "install".
- Helm release history is capped at the helm CLI's default (10 revisions); it was unlimited.
- The `chosen version:` line goes to stderr with the other status lines, so `platform diff` / `argocd diff`
  print only the diff on stdout.
- An empty diff says so instead of opening an empty pager.

## Changes from the bash menu (phase 4)

- The menu is `captain-utils menu`, and this binary opens it when run as `captain_utils` with no
  arguments. It needs a terminal; without one it says so and exits 1 (use the subcommands).
- No dev menu: the menu opens on the item list. `--captain-dir`, `--kube-context`, `--yes` and
  `--no-color` are passed to every item.
- No dev mode: `--env` and the `environment` variable are removed. A leftover `environment=dev` in a
  shell is ignored; a leftover `--env` is an unknown flag (exit 2). Everything runs the way production
  did, so `platform.yaml` is always required by `crds`, the `custom` warning always shows, and the
  GitHub release lists and `helm search` version lists are gone (`internal/github` with them).
- No legacy ArgoCD CRD install: `argocd` only diffs and runs `upgrade --install --skip-crds`, and
  applies no CRDs. The `--crd-version` and `--skip-crds-install` flags are removed. ArgoCD's CRDs come
  only from `crds`, so the order is `crds` → `argocd` → `crds`. A captain with a missing, duplicated or
  non-`vX.Y.Z` `platform_crds_version` gets a clear `crds` error (the menu item says it could not
  determine the version; `crds check` exits 2) instead of a silent fallback. `argocd_app_version` stays a pin for `versions diff`.
- A step that fails (or that you stop with Ctrl-C) prints its error and returns to the menu.
  In bash `set -e` ended the whole menu on most failures and Ctrl-C ended it everywhere. Esc or
  Ctrl-C at a menu still leaves it; `Exit` in the `aws` submenu still leaves the whole menu.
- `crds` offers the pin, `custom` or `Back`, then a confirm. A declined confirm (exit 3) is shown as
  skipped; any other failure says the CRDs were not applied, or only partly (the result line above it
  names which).
- No `helm repo update` at start: each item refreshes the repositories it uses.
- The codespace version check (`codespace_version` pin against `$VERSION`) is skipped, with a
  note, when there is no pin or no `$VERSION` (bash stopped on either). A mismatch asks first, as before.
- `show_diff_table` prints the table instead of opening a `gum table` viewer.
- `app_chart_preflight` is the one program still run as a program: it is a separately released
  tool (`app-chart-preflight`, or `$CAPTAIN_UTILS_PREFLIGHT`). The prompts, the list of
  ApplicationSets from `manifests/manifest.yaml` and the verdict messages are the bash menu's.
- `inspect_pods` is `pods watch`: Ctrl-C returns to the menu.

## Changes from the bash `crds` command (phase 3)

- Subcommands and flags instead of positional arguments: `crds check [version]`
  is `crds check [--version V]`, `check-dir DIR` / `apply-dir DIR` are
  `--chart-dir DIR`. `CRDS_AUTO_CONFIRM` is removed: scripts pass `--yes`
  (a variable left in a shell would silently apply CRDs). `CRDS_CHART` and `CRDS_WAIT_TIMEOUT` still work, as `--chart` / `--wait-timeout` (both take a bare number of seconds, as in bash, or `90s` / `2m`; a value that is neither is a usage error, exit 2, not a silent 120s).
- No helm, kubectl, yq, jq or gum: the Helm SDK renders the bundle (`helm
  template --include-crds -f platform.yaml`, `helm show crds` for the "83 of 85"
  count), client-go lists, patches and waits.
- `kubectl diff` is replaced by a server-side dry-run apply of each CRD compared
  with the live one (managedFields left out). The text is a unified diff headed
  `diff -u -N LIVE/... MERGED/...` rather than kubectl's temp-directory paths.
- The `kubectl apply` echo and its 3-line tail are replaced by one line saying
  how many CRDs are applied. Like kubectl, a failing CRD does not stop the others;
  every failure is reported together.
- Without a terminal, `check` and `apply` need `--version` or `--chart-dir` (exit 2 otherwise): bash
  `check` with no version read the pin. At a terminal `check` still reads the pin, and `apply` offers it.
- The kube client runs at 50 QPS / burst 100 (client-go's default is 5 / 10), for every command: `crds` makes
  about 90 sequential dry-run applies, which spent ~20s throttled.
- A bundle that declares the same CRD twice is refused, before anything is applied.
- The plan prints a `migrate:` line for CRDs an older installer applied client-side, and the diff cannot
  show what it does: the migration hands every field of the old record to our manager, and the server-side
  apply then **drops** the labels, annotations and `spec.conversion` the bundle does not declare
  (for example `helm.sh/chart`, or cert-manager's `inject-ca-from`). `keep:` only promises that the apply
  itself removes nothing it did not own.
- A single failed GET while waiting for CRDs is retried; three in a row end the wait.
- The menu's `target-version` is not a subcommand: `apply` resolves the version itself.
- `check` reports **drift** (exit 1) while a bundle CRD is being deleted. A terminating CRD still
  has its spec, so its diff is clean and the bash `check` called it "in sync" (exit 0) even though
  `apply` refuses to, and the type is about to disappear.
- On a cluster that has none of the bundle's CRDs, `check` says `87 not yet owned ... (87 not on
  the cluster yet)`, where the bash command said "all 87 owned".
- `check` also reports **drift** when a bundle CRD is not Established (another CRD owning the same
  kind leaves it `NamesAccepted=False`): the diff is clean, yet `apply` fails on it. The apply's
  Established timeout names the reason from the CRD's status.
- A CRD whose content matches but that our field manager does not own yet is **in sync** for `check`
  (exit 0), as it is for `apply`; the verdict says `(content); N CRD(s) not yet owned`. Pass
  `--require-owned` to make that exit 1.
- Before it applies, `apply` hands client-side-apply ownership over to the field manager (what
  `kubectl apply --server-side` does), so the `last-applied-configuration` annotation and the
  `kubectl-client-side-apply` manager are dropped from CRDs an older installer created. Checked
  against the bash command on a k3d captain: same end state.
- Ctrl-C or SIGTERM exits 130 with `interrupted`, after the in-flight call is cancelled (the bash
  command trapped the signal and cleaned its work dir; there is none now). Esc backs out of a
  prompt, as it did in gum, in every command.
- A group with no subcommand of that name (`crds apply-dir`, `platform bogus`) now exits 2 with
  `unknown command`. It used to print the group's help and exit 0, so a script calling a name
  that no longer exists did nothing and looked successful.

- `crds apply` ends with a result line (or `-o json`), and exits 3 when declined; see "Exit codes".
- `-o json` answers on stdout for usage errors and early failures too (exit codes unchanged), lists
  are `[]` instead of `null`, `crds apply -o json` has an `error` field, and `--no-diff` drops the diff text from
  an upgrade result (`diffLines` keeps its size).
- Fixed: `platform|argocd|calico upgrade` created objects the chart renders without `metadata.namespace`
  (the platform chart's Applications and AppProject) in the kubeconfig's default namespace instead of the
  release's, so ArgoCD reported `project glueops-core does not exist`. The release record was right; the objects were not.
- `CAPTAIN_UTILS_YES` is removed (only `--yes` / `-y` approves), like `CRDS_AUTO_CONFIRM`.

## Agent use (changes after phase 4)

- `pods list [--all] [-o json]` is a one-shot snapshot (the unhealthy pods, or every pod with `--all`);
  `-o json` prints an array of `{namespace, name, ready, status, restarts, age}` (`[]` when none).
  `pods watch` stays the refresh loop.
- `aws eks-addons|kubernetes|nodepools` take `-o json`: stdout is one
  `{file, dryRun, changed, written, changes: [{line, key, old, new}], diff, error}` and the diff text is in
  `diff`. Exit codes are unchanged, and a failure still prints the object with `error` set.
- `versions diff` accepts `-o text` (the table) like every other command; `table`, `csv` and `json` still work.
  An invalid `-o` value is a usage error (exit 2).
- A command line cobra rejects before the command runs (an unknown flag, say) prints
  `{"error": "..."}` on stdout, exit 2, when `-o json` is on the line. Without `-o json` stdout stays empty.

## Scripting and agents

Without a terminal every prompt fails fast with a hint, data goes to stdout and status to stderr.

| Command | Changes the cluster? | Without a terminal it needs |
|---|---|---|
| `versions diff`, `pods list`, `pods watch --once`, `crds check`, `platform diff`, `argocd diff` | no | `--version` or `--chart-dir` for the `crds`, `platform` and `argocd` ones |
| `crds apply`, `platform upgrade`, `argocd upgrade` | yes | `--version` (or `--chart-dir`) and `--yes` |
| `aws calico upgrade` | yes (deletes `aws-node`, installs calico) | `--yes` |
| `aws eks-addons\|kubernetes\|nodepools` | edits `main.tf` / `VERSIONS/aws.yaml` in the captain repo (`--dry-run` to preview) | nothing |

`pods watch` never returns without `--once`; use `pods list` for a snapshot. The whole production flow
without parsing text: `crds check --version V -o json`, `crds apply --version V --yes -o json`,
`argocd diff` / `argocd upgrade --version V --yes -o json`, `crds apply` again, `platform diff` /
`platform upgrade --version V --yes -o json`. Exit codes are in the table above.

### When a step fails or is interrupted

- `crds apply` partly failed or was interrupted: the result names the CRDs that failed (`-o json`:
  `failed`); CRDs after an interruption were not touched. Fix the cause and re-run `crds apply`; it is
  idempotent, and `crds check` says what is still out of sync.
- A helm upgrade failed (`"action": "failed"`): `helm status` / `helm history` on the release, then re-run the
  same command. A `pending-*` status means a helm lock left by an interrupted run: `helm rollback` to the last
  deployed revision (or wait for the other run to end) before re-running.
- `aws calico upgrade` failed after `aws-node` was removed: the cluster has no CNI until calico is
  installed, so re-run it (it skips the `aws-node` delete when it is already gone).

## Development

```sh
go test -race ./...
go test ./internal/aws -update   # rewrite the main.tf golden files after an intended change
CGO_ENABLED=0 go build -o bin/captain-utils .
```

Releases: Conventional Commits on `main` drive release-please; its tag runs
`.github/workflows/release-binaries.yaml`, which attaches
`captain-utils_<version>_linux_{amd64,arm64}.tar.gz` and `checksums.txt` to the
GitHub release.
