@AGENTS.md

## Handoff: venus k3d cluster (written 2026-11-26)

Working state for the next session. Everything here was verified in the previous one; re-check
anything that may have changed (cluster state above all). Delete this section when the work is done.

### Goal
Stand up the GlueOps platform for the `nonprod.venus.onglueops.rocks` captain on a local k3d cluster, using
`captain-utils` (this repo) instead of the codespace's bash `captain_utils`.

### Repos (clone these in the new container)
- `GlueOps/captain-utils`: this repo. PR #15, head branch `feat/crds`, CI green. It carries phases 3 and 4,
  `-o json` on check, upgrades and apply, `--no-diff`, and the helmx namespace fix.
- `development-captains/nonprod.venus.onglueops.rocks`: the captain. Use branch `chore/k3d-3-agents-eviction`
  (commit 2686faa, not on `main`): `k3d-config.yaml` with 3 agents and a 1% disk eviction threshold.
- `GlueOps/k3d`: the cluster-creation README, `k3d-config.yaml` and `add-machineid.sh`. Unchanged.

### Hard requirement: disk
The platform (glueops-platform v0.79.2) needs about 17 GB of images on 3 agents, because each node keeps its
own copy, on top of the codespace image and volumes. On a 31 GB disk the kubelets hit DiskPressure, pods were
evicted and ArgoCD never converged; host free space swung between 0 and 7 GB. 6 agents and 3 agents with 1%
thresholds both failed. Use a disk of about 50 GB or more. Check `lsblk` and `df -h /` BEFORE starting; a resize
may need a codespace restart to show up. If the disk cannot grow, the only fallback is 1 agent with trimmed replicas
(redis-ha at 1 in `argocd.yaml`, fewer in `platform.yaml`), which differs from production values.

### Rebuild order (captain repo root, a clean cluster)
Follow the `GlueOps/k3d` README: delete any old cluster, recreate the `k3d-glueops-net` network with MTU 1400,
`k3d cluster create --config k3d-config.yaml`, then run `add-machineid.sh`. Then, with a built `captain-utils`
(`export PATH=$HOME/sdk/go/bin:$PATH` if Go is not installed system-wide; `go build -o /tmp/cu .`):

1. `crds apply --version v0.1.6 --yes -o json`  (the captain's VERSIONS pin is a branch name, so pass the tag)
2. `argocd upgrade --version 9.3.7 --yes -o json --no-diff`, then wait for all argocd workloads to be Ready
3. `crds apply --version v0.1.6 --yes -o json` again (expect 0 applied)
4. `platform upgrade --version v0.79.2 --yes -o json --no-diff`

Versions come from `VERSIONS/glueops.yaml` in the captain repo. Without a terminal `--version` is mandatory.
After step 4, expect 28 Applications in `glueops-core` and none in `default`.

### Known facts and pitfalls
- A bug where objects without `metadata.namespace` landed in `default` is fixed (e1b4826). If Applications ever
  appear in `default` with `project glueops-core does not exist`, the binary predates that fix.
- `aws calico upgrade` is EKS-only. Never run it against k3d: it would replace flannel and break networking.
- Tenant demo apps (`antonios-tacos`, `keda-demo-*`, `whoami-protected`, `rabbitmq-nonprod`) come from the captain's
  config and may stay Degraded for non-disk reasons (secrets, tenant repo). Judge cluster health by the platform apps.
- A declined confirm is exit 3 and prints `"action": "declined"` with `-o json`; no terminal and no `--yes` is exit 1.
- Do not run `docker system prune` or delete images blindly: all k3d nodes and the codespace share one disk.
- Unit tests: `go test ./... -race`; also `gofmt -l .`, `go vet ./...`, `go mod tidy -diff` (CI runs these).

### Not done yet
- Chisel exit nodes for the k3d cluster: request them from the tools API named in the `GlueOps/k3d` README,
  then apply the output. The captain repo's `chisle.sh` holds example IPs and credentials committed in plain text;
  they are not ours to reuse, and should be rotated and removed from git.
- Vault configuration (the Terraform module step in the captain README), DNS and the tenant
  `deployment-configurations` repository.
- Deciding whether the 3-agent layout and eviction thresholds should go upstream into `GlueOps/k3d`.
- `aws calico upgrade -o json` was only tested against fakes, never for real.
