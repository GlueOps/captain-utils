// Package versions builds the current-vs-planned table (menu item
// show_diff_table): what the cluster and captain repo run now against what
// VERSIONS/*.yaml pins.
package versions

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/glueops/captain-utils/internal/aws"
	"github.com/glueops/captain-utils/internal/captain"
)

// NA is shown when a current version cannot be read.
const NA = "N/A"

// NotLive marks a current value that is not read from the cluster.
const NotLive = "*"

// Row is one line of the table.
type Row struct {
	Name    string `json:"name"`
	Current string `json:"current"`
	Planned string `json:"planned"`
}

// OK reports whether current matches planned.
func (r Row) OK() bool { return r.Current == r.Planned }

// Cluster is what the table reads from the cluster.
type Cluster interface {
	ServerVersion(ctx context.Context) (string, error)
	ReleaseChartVersion(ctx context.Context, release string) (string, error)
	DeploymentLabel(ctx context.Context, namespace, name, key string) (string, error)
	DeploymentImage(ctx context.Context, namespace, name string) (string, error)
}

// Collect builds the rows, in the order the Python helper printed them. Rows
// whose pin is missing are skipped with a note; rows that need
// terraform/kubernetes/main.tf are skipped when the captain has none (k3d,
// non-EKS), as are the addon rows without a local terraform.tfstate. A cluster
// read that fails shows N/A rather than failing the table.
func Collect(ctx context.Context, repo captain.Repo, cl Cluster, note func(string)) ([]Row, error) {
	glueops, err := repo.GlueOpsVersions()
	if err != nil {
		return nil, fmt.Errorf("read the pins (run from the captain repo root, or pass --captain-dir): %w", err)
	}
	awsV, err := repo.AWSVersions()
	hasAWS := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var tf []string
	if b, err := os.ReadFile(repo.MainTF()); err == nil {
		tf = aws.Lines(string(b))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	hasTF := tf != nil

	var rows []Row
	add := func(v captain.Versions, pin, name string, current func() string) {
		planned, ok := v.Get(pin)
		if !ok {
			note(pin + " not found in " + v.File)
			return
		}
		rows = append(rows, Row{Name: name, Current: current(), Planned: planned})
	}
	orNA := func(s string, err error) string {
		if err != nil || s == "" {
			return NA
		}
		return s
	}

	if hasAWS {
		state, err := aws.StateAddonVersions(repo.TFState())
		if err != nil {
			return nil, err
		}
		if state != nil {
			for _, k := range aws.AddonKeys {
				add(awsV, k, k, func() string { return orNA(state[k], nil) })
			}
		}
		if hasTF {
			add(awsV, captain.AMIReleaseVersion, "ami_release_version", func() string { return orNA(aws.CurrentAMI(tf), nil) })
		}
		add(awsV, captain.KubernetesVersion, "eks_version", func() string { return orNA(cl.ServerVersion(ctx)) })
	} else {
		note("no " + repo.Path("VERSIONS", "aws.yaml") + ": EKS rows left out")
	}

	add(glueops, captain.ArgocdChartVersion, "argocd_version", func() string { return orNA(cl.ReleaseChartVersion(ctx, "argocd")) })
	add(glueops, captain.ArgocdAppVersion, "argocd_app_version", func() string {
		return orNA(cl.DeploymentLabel(ctx, "glueops-core", "argocd-server", "app.kubernetes.io/version"))
	})
	add(glueops, captain.PlatformChartVersion, "glueops_platform_version", func() string {
		v, err := cl.ReleaseChartVersion(ctx, "glueops-platform")
		if err != nil || v == "" {
			return NA
		}
		return "v" + strings.TrimPrefix(v, "v")
	})
	add(glueops, captain.CalicoChartVersion, "calico_helm_chart_version", func() string { return orNA(cl.ReleaseChartVersion(ctx, "calico")) })
	add(glueops, captain.TigeraOperatorVersion, "tigera_operator_version", func() string {
		img, err := cl.DeploymentImage(ctx, "tigera-operator", "tigera-operator")
		_, tag, ok := strings.Cut(img, ":")
		if err != nil || !ok {
			return NA
		}
		return orNA(tag, nil)
	})
	// nothing on the cluster reports calicoctl's version (the Python helper showed
	// the calico chart version here, which is a different thing)
	add(glueops, captain.CalicoCtlVersion, "calico_ctl_version", func() string { return NA + NotLive })
	if hasTF {
		add(glueops, captain.TerraformModuleVer, "terraform_module_version", func() string { return orNA(aws.CurrentModuleRef(tf), nil) })
	} else {
		note("no " + repo.MainTF() + ": terraform and node pool rows left out")
	}
	return rows, nil
}
