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
| 4 | the interactive menu, installed as `captain_utils` | |

## Usage

Run from the captain repo root (or pass `--captain-dir`).

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
captain-utils platform diff                 # helm diff of the pinned glueops-platform (dev: --env dev, a chooser)
captain-utils platform upgrade              # diff (pager), confirm, helm upgrade --install
captain-utils platform upgrade --chart-dir ~/platform-helm-chart-platform   # "custom": a local chart
captain-utils argocd upgrade --version 9.3.7 --yes
captain-utils argocd upgrade --crd-version v3.2.12   # legacy CRD install (captains without platform_crds_version)
captain-utils aws calico upgrade
captain-utils pods watch                    # unhealthy pods, every 5s; --once, --all
```

```sh
captain-utils crds check                    # read-only; exit 0 in sync, 1 drift, 2 failure
captain-utils crds check --version v0.1.6
captain-utils crds apply                    # the VERSIONS/glueops.yaml pin (dev: a chooser): diff, confirm, apply
captain-utils crds apply --version v0.1.6 --yes
captain-utils crds apply --chart-dir ~/platform-crds   # "custom": a checkout rendered by hack/render.sh
```

`crds` reads `./platform.yaml` to pick the CRD profiles this cluster installs
(production requires it; dev without one installs every profile). Run it before
and after the `argocd` step. CRDs are only ever server-side applied
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
prints. Without a terminal, give the version on the command line and pass
`--yes`.

The `aws` main.tf commands edit `terraform/kubernetes/main.tf` and nothing else: review
the diff, commit, and run terraform as usual. They also accept the old menu
names (`upgrade-kubernetes`, `upgrade-eks-nodepools`).

### Global flags

| Flag | Env | Default |
|---|---|---|
| `--env production\|dev` | `environment` (as before), `CAPTAIN_UTILS_ENV` | `production` |
| `--captain-dir DIR` | `CAPTAIN_UTILS_CAPTAIN_DIR` | `.` |
| `--kube-context NAME` | `CAPTAIN_UTILS_KUBE_CONTEXT` | current context |
| `-y, --yes` | `CAPTAIN_UTILS_YES` | off |
| `--no-color` | `NO_COLOR` | off |

A flag beats its environment variable. Tables and diffs go to stdout, status
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
  repositories.yaml and cache as the CLI), client-go and the GitHub API.
  Verified on a k3d captain: `platform diff` and `argocd diff` are
  byte-identical to `helm diff upgrade` (a 3,378-line argo-cd diff included).
- Only the repo a flow uses is refreshed (the menu ran `helm repo update` for
  every repo at startup).
- The legacy ArgoCD CRD install (captains without `platform_crds_version`) reads
  `manifests/crds/kustomization.yaml` at the ref and server-side-applies each
  CRD (field manager `captain-utils`), instead of `kubectl apply -k`. It
  refuses a kustomization that uses anything but `resources`.
- `aws calico upgrade` asks before deleting the `aws-node` daemonset.
- An empty diff says so instead of opening an empty pager.

## Changes from the bash `crds` command (phase 3)

- Subcommands and flags instead of positional arguments: `crds check [version]`
  is `crds check [--version V]`, `check-dir DIR` / `apply-dir DIR` are
  `--chart-dir DIR`. `CRDS_AUTO_CONFIRM=yes` still works, as does `--yes`;
  `CRDS_CHART` and `CRDS_WAIT_TIMEOUT` still work, as `--chart` / `--wait-timeout` (a bare number is seconds, as in bash; `90s` and `2m` work too).
- No helm, kubectl, yq, jq or gum: the Helm SDK renders the bundle (`helm
  template --include-crds -f platform.yaml`, `helm show crds` for the "83 of 85"
  count), client-go lists, patches and waits.
- `kubectl diff` is replaced by a server-side dry-run apply of each CRD compared
  with the live one (managedFields left out). The text is a unified diff headed
  `diff -u -N LIVE/... MERGED/...` rather than kubectl's temp-directory paths.
- The `kubectl apply` echo and its 3-line tail are replaced by one line saying
  how many CRDs are applied. Like kubectl, a failing CRD does not stop the others;
  every failure is reported together.
- `check` with no version reads the pin and never opens the dev chooser, as before.
- The menu's `target-version` is not a subcommand: `apply` resolves the version itself.
- `check` reports **drift** (exit 1) while a bundle CRD is being deleted. A terminating CRD still
  has its spec, so its diff is clean and the bash `check` called it "in sync" (exit 0) even though
  `apply` refuses to, and the type is about to disappear.
- On a cluster that has none of the bundle's CRDs, `check` says `87 not yet owned ... (87 not on
  the cluster yet)`, where the bash command said "all 87 owned".
- Before it applies, `apply` hands client-side-apply ownership over to the field manager (what
  `kubectl apply --server-side` does), so the `last-applied-configuration` annotation and the
  `kubectl-client-side-apply` manager are dropped from CRDs an older installer created. Checked
  against the bash command on a k3d captain: same end state.
- Ctrl-C or SIGTERM exits 130 with `interrupted`, after the in-flight call is cancelled (the bash
  command trapped the signal and cleaned its work dir; there is none now). Esc backs out of a
  prompt, as it did in gum, in every command.
- A group with no subcommand of that name (`crds apply-dir`, `platform bogus`) now exits 1 with
  `unknown command`. It used to print the group's help and exit 0, so a script calling a name
  that no longer exists did nothing and looked successful.

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
