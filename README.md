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
| 3 | `crds` (platform-crds bundle, server-side apply) | next |
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
