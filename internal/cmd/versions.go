package cmd

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/glueops/captain-utils/internal/versions"
)

func (a *App) versionsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "versions",
		Short: "Compare what the captain runs with what VERSIONS/*.yaml pins",
	}
	var output string
	diff := &cobra.Command{
		Use:   "diff",
		Short: "Table of current vs planned versions (menu: show_diff_table)",
		Long: `Table of current vs planned versions (menu item show_diff_table).

Planned comes from VERSIONS/glueops.yaml and VERSIONS/aws.yaml. Current comes
from the cluster (Helm releases, Deployments, the API server version), from
terraform/kubernetes/main.tf (AMI, terraform module ref) and from a local
terraform.tfstate (EKS addons). Rows whose source is missing are left out
with a note. Read-only: unlike the old helper it never rewrites main.tf.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if output == "text" { // the name the other commands use for the human form
				output = "table"
			}
			if output != "table" && output != "csv" && output != "json" {
				return &ExitError{Code: ExitUsage, Err: fmt.Errorf("-o must be text, table, csv or json, got %q", output)}
			}
			cl, err := a.cluster()
			if err != nil {
				if output == "json" {
					a.writeJSONError(err)
				}
				return err
			}
			rows, err := versions.Collect(cmd.Context(), a.repo(), cl, func(s string) { a.p.Note("%s", s) })
			if err != nil {
				if output == "json" {
					a.writeJSONError(err)
				}
				return err
			}
			return a.writeVersions(output, rows)
		},
	}
	diff.Flags().StringVarP(&output, "output", "o", "table", "text (or table), csv or json")
	c.AddCommand(diff)
	return c
}

func status(r versions.Row) string {
	if r.OK() {
		return "✅"
	}
	return "❌"
}

func (a *App) writeVersions(output string, rows []versions.Row) error {
	switch output {
	case "json":
		type row struct {
			versions.Row
			OK bool `json:"ok"`
		}
		out := make([]row, len(rows))
		for i, r := range rows {
			out[i] = row{r, r.OK()}
		}
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	case "csv":
		w := csv.NewWriter(a.Out)
		_ = w.Write([]string{"Tool Name", "Current", "Planned", "Status"})
		for _, r := range rows {
			_ = w.Write([]string{r.Name, r.Current, r.Planned, status(r)})
		}
		w.Flush()
		return w.Error()
	}
	cells := make([][]string, len(rows))
	notLive := false
	for i, r := range rows {
		cells[i] = []string{r.Name, r.Current, r.Planned, status(r)}
		notLive = notLive || strings.Contains(r.Current, versions.NotLive)
	}
	if notLive {
		a.p.Warn("Note: a value with an asterisk (*) is not a live version")
	}
	a.p.Table([]string{"Tool Name", "Current", "Planned", "Status"}, cells)
	return nil
}
