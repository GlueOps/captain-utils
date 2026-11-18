// Package aws holds the EKS captain flows: the terraform/kubernetes/main.tf
// edits (addons, node pools, Kubernetes version), the local tfstate reader and
// (later) the calico upgrade.
//
// The main.tf edits are a port of the line-based edits the codespace's Python
// helper (script_captain_utils) made, kept line-based on purpose: main.tf is
// operator-owned, so the edit must change exactly the values it names and leave
// every other byte (comments, alignment, commented-out pools) alone.
package aws

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Lines splits text into lines that keep their "\n", so Join(Lines(s)) == s.
func Lines(s string) []string {
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// Join is the inverse of Lines.
func Join(lines []string) string { return strings.Join(lines, "") }

// Change is one value an edit replaced.
type Change struct {
	Line     int // 1-based, in the edited file
	Key      string
	Old, New string
}

// assignValue matches `key = "value"` the way the Python helper did: the first
// `= "` on the line and the next `"` after it.
func assignValue(line string) (start, end int, ok bool) {
	i := strings.Index(line, `= "`)
	if i < 0 {
		return 0, 0, false
	}
	start = i + 3
	j := strings.IndexByte(line[start:], '"')
	if j < 0 {
		return 0, 0, false
	}
	return start, start + j, true
}

func splice(line string, start, end int, v string) string { return line[:start] + v + line[end:] }

// SetAssignments sets the quoted value of every line that mentions one of the
// keys (`csi_driver_version = "..."`). When a line mentions several keys the
// last one in keys wins, as in the Python helper. Keys with an empty target are
// left alone.
func SetAssignments(lines []string, keys []string, targets map[string]string) ([]string, []Change) {
	out := make([]string, len(lines))
	var changes []Change
	for i, line := range lines {
		out[i] = line
		key := ""
		for _, k := range keys {
			if targets[k] != "" && strings.Contains(line, k) {
				key = k
			}
		}
		if key == "" {
			continue
		}
		s, e, ok := assignValue(line)
		if !ok || line[s:e] == targets[key] {
			continue
		}
		changes = append(changes, Change{Line: i + 1, Key: key, Old: line[s:e], New: targets[key]})
		out[i] = splice(line, s, e, targets[key])
	}
	return out, changes
}

// AddonKeys are the EKS addon pins, in the order the Python helper applied them.
var AddonKeys = []string{"csi_driver_version", "coredns_version", "kube_proxy_version"}

// UpgradeAddons sets the addon versions in main.tf.
func UpgradeAddons(lines []string, targets map[string]string) ([]string, []Change) {
	return SetAssignments(lines, AddonKeys, targets)
}

// UpgradeKubernetes sets eks_version in main.tf.
func UpgradeKubernetes(lines []string, version string) ([]string, []Change) {
	return SetAssignments(lines, []string{"eks_version"}, map[string]string{"eks_version": version})
}

var (
	poolName       = regexp.MustCompile(`^\s*#?\s*"name"\s*:\s*"([^"]+)`)
	poolAMI        = regexp.MustCompile(`^\s*"ami_release_version"\s*:\s*"([^"]+)"`)
	poolAMIAny     = regexp.MustCompile(`^\s*#?\s*"ami_release_version"\s*:\s*"([^"]+)"`)
	poolKubernetes = regexp.MustCompile(`^\s*#?\s*"kubernetes_version"\s*:\s*"(\d+\.\d+)"`)
	poolSuffix     = regexp.MustCompile(`^(.*-)(\d+)$`)
	moduleRef      = regexp.MustCompile(`ref=(v[0-9]+\.[0-9]+\.[0-9]+)`)
)

// NodePoolRange returns the half-open line range [start, end) holding the node
// pool objects: from the line after the last "node_pools" line up to the
// list's closing "]", the last non-blank line before the last "peering_configs"
// line. (The Python helper took the line right before peering_configs as the
// bracket, so a blank line between them copied the "]" into the list.)
func NodePoolRange(lines []string) (start, end int, err error) {
	start, peering := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "peering_configs") {
			peering = i
		}
		if strings.Contains(l, "node_pools") {
			start = i + 1
		}
	}
	switch {
	case start < 0:
		return 0, 0, errors.New(`no "node_pools" line in main.tf`)
	case peering < 0:
		return 0, 0, errors.New(`no "peering_configs" line in main.tf (the node pool list must be followed by peering_configs)`)
	case peering < start:
		return 0, 0, errors.New(`"node_pools" must come before "peering_configs" in main.tf`)
	}
	end = peering - 1
	for end >= start && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end < start || strings.TrimSpace(lines[end]) != "]" {
		return 0, 0, fmt.Errorf(`main.tf line %d: expected the node_pools list's closing "]" before peering_configs`, end+1)
	}
	if end == start {
		return 0, 0, errors.New("node_pools is empty: there is no pool to copy")
	}
	return start, end, nil
}

// AddNodePools copies every node pool between node_pools and peering_configs
// and appends the copies to the list: each copy's name has its trailing -N
// incremented, its ami_release_version set to ami and its kubernetes_version set
// to kubernetesVersion (the cluster's current major.minor, so the new pools join
// at the version the control plane runs). The old pools are kept: deleting them
// once the new ones are ready is the operator's call.
func AddNodePools(lines []string, ami, kubernetesVersion string) ([]string, []Change, error) {
	if ami == "" {
		return nil, nil, errors.New("no ami_release_version target")
	}
	if kubernetesVersion == "" {
		return nil, nil, errors.New("no Kubernetes version for the new node pools")
	}
	start, end, err := NodePoolRange(lines)
	if err != nil {
		return nil, nil, err
	}
	var copied []string
	var changes []Change
	insertedAt := end + 1 // 1-based line of the first copied line in the result
	for i, line := range lines[start:end] {
		outLine := insertedAt + i
		if m := poolName.FindStringSubmatchIndex(line); m != nil {
			old := line[m[2]:m[3]]
			sm := poolSuffix.FindStringSubmatch(old)
			if sm == nil {
				return nil, nil, fmt.Errorf("main.tf line %d: node pool name %q does not end in -N, so the new pool cannot be named", start+i+1, old)
			}
			n, err := strconv.Atoi(sm[2])
			if err != nil {
				return nil, nil, fmt.Errorf("main.tf line %d: node pool name %q: %w", start+i+1, old, err)
			}
			nv := sm[1] + strconv.Itoa(n+1)
			line = splice(line, m[2], m[3], nv)
			changes = append(changes, Change{Line: outLine, Key: "name", Old: old, New: nv})
		}
		if m := poolAMI.FindStringSubmatchIndex(line); m != nil {
			changes = append(changes, Change{Line: outLine, Key: "ami_release_version", Old: line[m[2]:m[3]], New: ami})
			line = splice(line, m[2], m[3], ami)
		}
		if m := poolKubernetes.FindStringSubmatchIndex(line); m != nil {
			changes = append(changes, Change{Line: outLine, Key: "kubernetes_version", Old: line[m[2]:m[3]], New: kubernetesVersion})
			line = splice(line, m[2], m[3], kubernetesVersion)
		}
		copied = append(copied, line)
	}
	out := make([]string, 0, len(lines)+len(copied))
	out = append(out, lines[:end]...)
	// the last existing pool's closing brace gets a comma so the copies follow it
	last := out[end-1]
	body := strings.TrimRight(last, " \t\r\n")
	if strings.HasSuffix(body, "}") {
		out[end-1] = body + "," + last[len(body):]
	}
	out = append(out, copied...)
	out = append(out, lines[end:]...)
	return out, changes, nil
}

// CurrentAMI is the last ami_release_version in main.tf (commented-out pools included, as before).
func CurrentAMI(lines []string) string { return lastMatch(poolAMIAny, lines) }

// CurrentModuleRef is the last ?ref=vX.Y.Z in main.tf: the terraform module version.
func CurrentModuleRef(lines []string) string { return lastMatch(moduleRef, lines) }

func lastMatch(re *regexp.Regexp, lines []string) string {
	v := ""
	for _, l := range lines {
		if m := re.FindStringSubmatch(l); m != nil {
			v = m[1]
		}
	}
	return v
}
