package upgrade

import (
	"encoding/json"
	"io"

	"github.com/glueops/captain-utils/internal/helmx"
)

// What a run did, for Result.Action.
const (
	ActionNone      = "none"      // nothing was changed: stopped before the upgrade (a diff with no flag, or an error)
	ActionFailed    = "failed"    // helm upgrade --install was started and failed: check the release before re-running
	ActionDiff      = "diff"      // a diff command: showed the diff, changed nothing
	ActionDeclined  = "declined"  // the confirmation was answered no (exit 3)
	ActionInstalled = "installed" // helm upgrade --install created the release
	ActionUpgraded  = "upgraded"  // helm upgrade --install updated the release
)

// Result is what a platform, argocd or calico run did, for scripts (-o json).
type Result struct {
	Release   string `json:"release"`
	Namespace string `json:"namespace"`
	Chart     string `json:"chart"`
	Version   string `json:"version"` // empty for a local --chart-dir
	Action    string `json:"action"`
	// Changed is whether the diff showed changes; null when the run stopped before the diff.
	Changed *bool  `json:"changed"`
	Diff    string `json:"diff"` // the helm diff; empty with --no-diff
	// DiffLines is the length of the helm diff, kept when --no-diff leaves the text out.
	DiffLines int    `json:"diffLines"`
	Status    string `json:"status"`   // helm release status after an upgrade
	Revision  int    `json:"revision"` // helm revision after an upgrade
	Notes     string `json:"notes"`
	// Warnings are things that went wrong without stopping the run (calico: aws-node could not be deleted).
	Warnings []string `json:"warnings"`
	// Error is the failure's message when the run failed (exit 1) or was declined (exit 3).
	Error string `json:"error"`
}

// NewResult starts a result: nothing done yet.
func NewResult() *Result { return &Result{Action: ActionNone, Warnings: []string{}} }

func (r *Result) release(rel helmx.Release) {
	if r == nil {
		return
	}
	r.Release, r.Namespace, r.Chart, r.Version = rel.Name, rel.Namespace, rel.Chart, rel.Version
}

func (r *Result) warn(msg string) {
	if r != nil {
		r.Warnings = append(r.Warnings, msg)
	}
}

// WriteJSON prints the result as one JSON object.
func (r *Result) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
