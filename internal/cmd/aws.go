package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pmezard/go-difflib/difflib"
	"github.com/spf13/cobra"

	"github.com/glueops/captain-utils/internal/aws"
	"github.com/glueops/captain-utils/internal/captain"
)

func (a *App) awsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "aws",
		Short: "EKS captains: main.tf edits to the VERSIONS/aws.yaml pins, and calico",
		Long: `EKS captains (menu item aws).

eks-addons, kubernetes and nodepools edit terraform/kubernetes/main.tf to the
VERSIONS/aws.yaml pins: each prints the diff and writes main.tf (or only prints
it with --dry-run). They change nothing on the cluster: review the diff,
commit, and run terraform as usual.

calico upgrade replaces the EKS VPC CNI with calico on the cluster.`,
	}
	var dryRun bool
	var output string
	dry := func(cmd *cobra.Command) *cobra.Command {
		cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the diff, do not write main.tf")
		cmd.Flags().StringVarP(&output, "output", "o", "text", "result format: text or json (stdout is then only the JSON: {file, dryRun, changed, written, changes, diff, error})")
		return cmd
	}

	c.AddCommand(dry(&cobra.Command{
		Use:     "eks-addons",
		Short:   "Set csi_driver_version, coredns_version and kube_proxy_version",
		Args:    cobra.NoArgs,
		Aliases: []string{"addons"},
		RunE: func(*cobra.Command, []string) error {
			return a.awsRun(output, func(res *awsResult) error {
				pins, err := a.repo().AWSVersions()
				if err != nil {
					return err
				}
				targets := map[string]string{}
				for _, k := range aws.AddonKeys {
					if v, ok := pins.Get(k); ok {
						targets[k] = v
					} else {
						a.p.Warn("%s not found in %s: left as is", k, pins.File)
					}
				}
				return a.editMainTF(res, dryRun, func(l []string) ([]string, []aws.Change, error) {
					out, ch := aws.UpgradeAddons(l, targets)
					return out, ch, nil
				})
			})
		},
	}))

	c.AddCommand(dry(&cobra.Command{
		Use:     "kubernetes",
		Short:   "Set eks_version to kubernetes_version (the control plane upgrade)",
		Args:    cobra.NoArgs,
		Aliases: []string{"upgrade-kubernetes"},
		RunE: func(*cobra.Command, []string) error {
			return a.awsRun(output, func(res *awsResult) error {
				pins, err := a.repo().AWSVersions()
				if err != nil {
					return err
				}
				target, err := pins.Require(captain.KubernetesVersion)
				if err != nil {
					return err
				}
				a.p.Info("eks_version -> %s", target)
				return a.editMainTF(res, dryRun, func(l []string) ([]string, []aws.Change, error) {
					out, ch := aws.UpgradeKubernetes(l, target)
					return out, ch, nil
				})
			})
		},
	}))

	var poolK8s string
	nodepools := &cobra.Command{
		Use:   "nodepools",
		Short: "Add a copy of every node pool at ami_release_version (name -N becomes -N+1)",
		Long: `Add a copy of every node pool at ami_release_version.

Each pool between node_pools and peering_configs is copied: the copy's name
ends in -N+1, its ami_release_version is the VERSIONS/aws.yaml pin and its
kubernetes_version is the version the control plane runs now (read from the
cluster, or --kubernetes-version). The old pools are kept; remove them once
workloads have moved.`,
		Args:    cobra.NoArgs,
		Aliases: []string{"upgrade-eks-nodepools"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.awsRun(output, func(res *awsResult) error {
				pins, err := a.repo().AWSVersions()
				if err != nil {
					return err
				}
				ami, err := pins.Require(captain.AMIReleaseVersion)
				if err != nil {
					return err
				}
				if poolK8s == "" {
					cl, err := a.cluster()
					if err != nil {
						return err
					}
					if poolK8s, err = cl.ServerVersion(cmd.Context()); err != nil {
						return fmt.Errorf("%w (or pass --kubernetes-version)", err)
					}
				}
				a.p.Info("new node pools: ami_release_version %s, kubernetes_version %s", ami, poolK8s)
				return a.editMainTF(res, dryRun, func(l []string) ([]string, []aws.Change, error) {
					return aws.AddNodePools(l, ami, poolK8s)
				})
			})
		},
	}
	nodepools.Flags().StringVar(&poolK8s, "kubernetes-version", "", "kubernetes_version of the new pools (default: the cluster's current major.minor)")
	c.AddCommand(dry(nodepools), a.calicoCmd())
	return c
}

// editMainTF applies edit to main.tf, prints the unified diff, and writes the
// result unless dryRun. The file is replaced atomically with its mode kept.
func (a *App) editMainTF(res *awsResult, dryRun bool, edit func([]string) ([]string, []aws.Change, error)) error {
	path := a.repo().MainTF()
	if res != nil {
		res.File, res.DryRun = path, dryRun
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w (aws commands need an EKS captain repo)", err)
	}
	before := aws.Lines(string(b))
	after, changes, err := edit(before)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if len(changes) == 0 {
		a.p.Success("%s already matches the pins, nothing to change", path)
		return nil
	}
	d, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: before, B: after, FromFile: path, ToFile: path, Context: 3})
	if err != nil {
		return err
	}
	if res != nil {
		res.Changed, res.Diff = true, d
		for _, c := range changes {
			res.Changes = append(res.Changes, awsChange{Line: c.Line, Key: c.Key, Old: c.Old, New: c.New})
		}
	} else {
		fmt.Fprint(a.Out, d)
	}
	if dryRun {
		a.p.Note("--dry-run: %s not written (%d change(s))", path, len(changes))
		return nil
	}
	if err := writeAtomic(path, []byte(aws.Join(after))); err != nil {
		return err
	}
	if res != nil {
		res.Written = true
	}
	a.p.Success("wrote %s (%d change(s)); review, commit, then terraform plan/apply", path, len(changes))
	return nil
}

// awsResult is the -o json answer of the main.tf commands.
type awsResult struct {
	File    string      `json:"file"`
	DryRun  bool        `json:"dryRun"`
	Changed bool        `json:"changed"`
	Written bool        `json:"written"`
	Changes []awsChange `json:"changes"`
	Diff    string      `json:"diff"`
	Error   string      `json:"error,omitempty"`
}

type awsChange struct {
	Line int    `json:"line"`
	Key  string `json:"key"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// awsRun runs a main.tf command. With -o json the diff goes into the result and stdout is only
// the JSON, also when the command fails (error set), as for the upgrade commands.
func (a *App) awsRun(output string, run func(*awsResult) error) error {
	switch output {
	case "text":
		return run(nil)
	case "json":
	default:
		return &ExitError{Code: ExitUsage, Err: fmt.Errorf("--output must be text or json, got %q", output)}
	}
	res := &awsResult{Changes: []awsChange{}}
	err := run(res)
	if err != nil {
		res.Error = err.Error()
	}
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	if jerr := enc.Encode(res); jerr != nil && err == nil {
		err = jerr
	}
	return err
}

func writeAtomic(path string, data []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
