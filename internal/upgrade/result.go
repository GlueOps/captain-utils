package upgrade

import (
	"encoding/json"
	"io"

	"github.com/glueops/captain-utils/internal/helmx"
)

// What a run did, for Result.Action.
const (
	ActionNone      = "none"      // nothing was changed: stopped before the upgrade (a diff with no flag, or an error)
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
	Version   string `json:"version,omitempty"` // empty for a local --chart-dir
	Action    string `json:"action"`
	// Changed is whether the diff showed changes; absent when the run stopped before the diff.
	Changed *bool  `json:"changed,omitempty"`
	Diff    string `json:"diff,omitempty"` // the helm diff; empty with --no-diff
	// DiffLines is the length of the helm diff, kept when --no-diff leaves the text out.
	DiffLines int    `json:"diffLines,omitempty"`
	Status    string `json:"status,omitempty"`   // helm release status after an upgrade
	Revision  int    `json:"revision,omitempty"` // helm revision after an upgrade
	Notes     string `json:"notes,omitempty"`
	// Error is the failure's message when the run failed (exit 1) or was declined (exit 3).
	Error string `json:"error,omitempty"`
}

// NewResult starts a result: nothing done yet.
func NewResult() *Result { return &Result{Action: ActionNone} }

func (r *Result) release(rel helmx.Release) {
	if r == nil {
		return
	}
	r.Release, r.Namespace, r.Chart, r.Version = rel.Name, rel.Namespace, rel.Chart, rel.Version
}

// WriteJSON prints the result as one JSON object.
func (r *Result) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
