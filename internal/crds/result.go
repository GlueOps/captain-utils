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
}

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
func (r *Result) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
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
