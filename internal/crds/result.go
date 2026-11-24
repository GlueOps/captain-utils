package crds

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Failure is one CRD that did not apply.
type Failure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// Result is what an apply did, for scripts: which CRDs it applied (content changed),
// which it left unchanged (applied or skipped with no content change), which failed, and
// whether the post-apply wait saw every bundle CRD Established. A declined or in-sync run
// applies nothing: Applied is empty and Established false.
type Result struct {
	Version     string    `json:"version"`
	Applied     []string  `json:"applied"`
	Unchanged   []string  `json:"unchanged"`
	Failed      []Failure `json:"failed"`
	Established bool      `json:"established"`
	// Error is why the run did not finish: a failure (exit 1 or 2) or a declined confirm (exit 3).
	Error string `json:"error,omitempty"`
}

// NewResult is an empty result for a run that stopped before it looked at any CRD.
func NewResult(version string) *Result { return newResult(version) }

func newResult(version string) *Result {
	return &Result{Version: version, Applied: []string{}, Unchanged: []string{}, Failed: []Failure{}}
}

// Summary is the fixed one-line form: applied=N unchanged=N failed=N.
func (r *Result) Summary() string {
	return fmt.Sprintf("applied=%d unchanged=%d failed=%d", len(r.Applied), len(r.Unchanged), len(r.Failed))
}

// WriteText prints the summary line and one line per failed CRD.
func (r *Result) WriteText(w io.Writer) {
	fmt.Fprintln(w, r.Summary())
	for _, f := range r.Failed {
		fmt.Fprintf(w, "failed: %s: %s\n", f.Name, f.Error)
	}
}

// WriteJSON prints the result as one JSON object.
func (r *Result) WriteJSON(w io.Writer) error { return writeJSON(w, r) }

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// settle files the bundle's CRDs into Applied / Unchanged after an apply: applied are the
// ones that applied with a content change (changed), unchanged the rest that applied.
func (r *Result) settle(all, changed, failed []string) {
	chg, bad := map[string]bool{}, map[string]bool{}
	for _, n := range changed {
		chg[n] = true
	}
	for _, n := range failed {
		bad[n] = true
	}
	for _, n := range all {
		switch {
		case bad[n]:
		case chg[n]:
			r.Applied = append(r.Applied, n)
		default:
			r.Unchanged = append(r.Unchanged, n)
		}
	}
	sort.Strings(r.Applied)
	sort.Strings(r.Unchanged)
}

func failedNames(fs []Failure) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

func failureText(fs []Failure) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, f.Error)
	}
	return strings.Join(parts, "\n")
}

// CheckReport is what a check found, for scripts (check -o json). InSync is true exactly when
// check exits 0. Reasons says why not, in the words of the drift warning. NotOwned and
// NotOnCluster only make it drift with --require-owned.
type CheckReport struct {
	Version        string   `json:"version"`
	InSync         bool     `json:"inSync"`
	Total          int      `json:"total"`
	New            int      `json:"new"`
	Changed        int      `json:"changed"`
	Differs        []string `json:"differs"`
	Terminating    []string `json:"terminating"`
	NotEstablished []string `json:"notEstablished"`
	NotOwned       []string `json:"notOwned"`
	NotOnCluster   int      `json:"notOnCluster"`
	Reasons        []string `json:"reasons"`
	// Error is set instead of the findings when the check itself failed (exit 2).
	Error string `json:"error,omitempty"`
}

// NewCheckReport is an empty report: every list is [] so a script can iterate it.
func NewCheckReport(version string, total int) *CheckReport { return newCheckReport(version, total) }

func newCheckReport(version string, total int) *CheckReport {
	return &CheckReport{
		Version: version, Total: total,
		Differs: []string{}, Terminating: []string{}, NotEstablished: []string{}, NotOwned: []string{}, Reasons: []string{},
	}
}

// WriteJSON prints the report as one JSON object.
// Lists that were never set print as [], not null.
func (r *CheckReport) WriteJSON(w io.Writer) error {
	for _, l := range []*[]string{&r.Differs, &r.Terminating, &r.NotEstablished, &r.NotOwned, &r.Reasons} {
		*l = nonNil(*l)
	}
	return writeJSON(w, r)
}
