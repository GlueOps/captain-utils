package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"
)

// PodRow is one line of `kubectl get pods -A`.
type PodRow struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Ready     string `json:"ready"`
	Status    string `json:"status"`
	Restarts  int32  `json:"restarts"`
	Age       string `json:"age"`
}

// String formats the row like kubectl's columns.
func (r PodRow) String() string {
	return fmt.Sprintf("%-20s %-55s %-7s %-26s %-8d %s", r.Namespace, r.Name, r.Ready, r.Status, r.Restarts, r.Age)
}

// PodHeader is the column header for PodRow.String.
var PodHeader = fmt.Sprintf("%-20s %-55s %-7s %-26s %-8s %s", "NAMESPACE", "NAME", "READY", "STATUS", "RESTARTS", "AGE")

// Pods lists every pod in the cluster as kubectl shows it.
func (c *Client) Pods(ctx context.Context, now time.Time) ([]PodRow, error) {
	list, err := c.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	rows := make([]PodRow, 0, len(list.Items))
	for i := range list.Items {
		rows = append(rows, PodRowOf(&list.Items[i], now))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

// PodRowOf computes kubectl's READY, STATUS and RESTARTS columns for a pod
// (the same precedence as kubectl's printPod: pod reason, then init
// containers, then containers, then deletion).
func PodRowOf(pod *corev1.Pod, now time.Time) PodRow {
	var restarts, sidecarRestarts int32
	ready := 0
	// native sidecars: init containers with restartPolicy Always keep running next to the
	// containers, so (as in kubectl) they count in READY and their restarts in RESTARTS
	total := len(pod.Spec.Containers)
	sidecar := map[string]bool{}
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecar[c.Name] = true
			total++
		}
	}
	reason := string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}
	initializing := false
	for i, cs := range pod.Status.InitContainerStatuses {
		restarts += cs.RestartCount
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			continue
		case sidecar[cs.Name] && cs.Started != nil && *cs.Started:
			sidecarRestarts += cs.RestartCount
			if cs.Ready {
				ready++
			}
			continue
		case cs.State.Terminated != nil:
			switch {
			case cs.State.Terminated.Reason != "":
				reason = "Init:" + cs.State.Terminated.Reason
			case cs.State.Terminated.Signal != 0:
				reason = fmt.Sprintf("Init:Signal:%d", cs.State.Terminated.Signal)
			default:
				reason = fmt.Sprintf("Init:ExitCode:%d", cs.State.Terminated.ExitCode)
			}
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + cs.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(pod.Spec.InitContainers))
		}
		initializing = true
		break
	}
	if !initializing || podInitialized(pod) {
		restarts = sidecarRestarts
		hasRunning := false
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			cs := pod.Status.ContainerStatuses[i]
			restarts += cs.RestartCount
			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				reason = cs.State.Waiting.Reason
			case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
				reason = cs.State.Terminated.Reason
			case cs.State.Terminated != nil && cs.State.Terminated.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", cs.State.Terminated.Signal)
			case cs.State.Terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
			case cs.Ready && cs.State.Running != nil:
				hasRunning = true
				ready++
			}
		}
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}
	if pod.DeletionTimestamp != nil {
		if pod.Status.Reason == "NodeLost" {
			reason = "Unknown"
		} else {
			reason = "Terminating"
		}
	}
	return PodRow{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Ready:     fmt.Sprintf("%d/%d", ready, total),
		Status:    reason,
		Restarts:  restarts,
		Age:       duration.HumanDuration(now.Sub(pod.CreationTimestamp.Time)),
	}
}

// podInitialized is the Initialized=True condition.
func podInitialized(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodInitialized {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// UnhealthyStatuses are the statuses the inspect_pods menu item watched for.
var UnhealthyStatuses = []string{"Pending", "CrashLoopBackOff", "Error", "ContainerCreating", "ImagePullBackOff", "ErrImagePull"}

// Unhealthy reports whether a row's status contains one of UnhealthyStatuses
// (substring, as the old grep did, so Init:CrashLoopBackOff counts).
func Unhealthy(r PodRow) bool {
	for _, s := range UnhealthyStatuses {
		if strings.Contains(r.Status, s) {
			return true
		}
	}
	return false
}
