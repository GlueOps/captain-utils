package helmx

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
)

// CRDBundle is a chart's CRDs, rendered.
type CRDBundle struct {
	// Manifest is the CRD documents. With a values file it is `helm template
	// --include-crds -f <values>` (the profile-selected subset); without one,
	// `helm show crds` (every CRD the chart ships).
	Manifest []byte
	// Union is how many CRDs the chart ships whatever the values select
	// (`helm show crds | grep -c '^kind: CustomResourceDefinition'`).
	Union int
	// Chart is "name version" of the chart that was rendered.
	Chart string
}

var crdKindLine = regexp.MustCompile(`(?m)^kind: CustomResourceDefinition`)

// CountCRDKinds counts the lines `kind: CustomResourceDefinition` in a manifest,
// the way the bash crds command's grep -c did.
func CountCRDKinds(manifest []byte) int { return len(crdKindLine.FindAll(manifest, -1)) }

// showCRDs is `helm show crds`: every CRD file of the chart and its
// dependencies, one document each, whatever the values select.
func showCRDs(ch *chart.Chart) []byte {
	var b bytes.Buffer
	for _, crd := range ch.CRDObjects() {
		if !bytes.HasPrefix(bytes.TrimLeft(crd.File.Data, " \t\r\n"), []byte("---")) {
			b.WriteString("---\n")
		}
		b.Write(crd.File.Data)
		b.WriteString("\n")
	}
	return b.Bytes()
}

// CRDBundle loads a chart (an oci:// or repo reference, or a local directory
// when version is empty) and renders its CRDs. valuesFile selects the CRD
// profiles; "" renders every CRD. A values file that does not satisfy the
// chart's values.schema.json is an error, never a silent fallback.
func (c *Client) CRDBundle(ctx context.Context, chartRef, version, valuesFile string) (*CRDBundle, error) {
	cfg, err := c.config("default")
	if err != nil {
		return nil, err
	}
	inst := action.NewInstall(cfg)
	ch, err := c.loadChart(Release{Chart: chartRef, Version: version}, &inst.ChartPathOptions)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", chartRef, err)
	}
	// helm show crds comes first: rendering below prunes disabled subcharts out of ch.
	all := showCRDs(ch)
	out := &CRDBundle{Manifest: all, Union: CountCRDKinds(all), Chart: ch.Metadata.Name + " " + ch.Metadata.Version}
	if valuesFile == "" {
		return out, nil
	}
	vals, err := c.values(Release{ValueFiles: []string{valuesFile}})
	if err != nil {
		return nil, err
	}
	inst.ReleaseName = "x"
	inst.Namespace = "default"
	inst.DryRun = true
	inst.DryRunOption = "client"
	inst.ClientOnly = true
	inst.Replace = true
	inst.IncludeCRDs = true
	rel, err := inst.RunWithContext(ctx, ch, vals)
	if err != nil {
		return nil, fmt.Errorf("select CRD profiles from %s: %w", valuesFile, err)
	}
	out.Manifest = []byte(strings.TrimSpace(rel.Manifest) + "\n")
	return out, nil
}
