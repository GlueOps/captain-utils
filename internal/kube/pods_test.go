package kube

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodRowOf(t *testing.T) {
	now := time.Now()
	created := metav1.NewTime(now.Add(-90 * time.Second))
	two := corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}, InitContainers: []corev1.Container{{Name: "i"}}}
	waiting := func(r string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: r}}
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}
	always := corev1.ContainerRestartPolicyAlways
	started := true
	sidecarSpec := corev1.PodSpec{Containers: two.Containers, InitContainers: []corev1.Container{{Name: "side", RestartPolicy: &always}}}
	for name, tc := range map[string]struct {
		status    corev1.PodStatus
		deleting  bool
		want      string
		ready     string
		unhealthy bool
		spec      *corev1.PodSpec
		restarts  int32
	}{
		"running": {status: corev1.PodStatus{Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{State: done}},
			ContainerStatuses:     []corev1.ContainerStatus{{Ready: true, State: running}, {Ready: true, State: running, RestartCount: 2}}},
			want: "Running", ready: "2/2"},
		"crashloop": {status: corev1.PodStatus{Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{State: done}},
			ContainerStatuses:     []corev1.ContainerStatus{{Ready: true, State: running}, {State: waiting("CrashLoopBackOff")}}},
			want: "CrashLoopBackOff", ready: "1/2", unhealthy: true},
		"init image pull": {status: corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{State: waiting("ErrImagePull")}}},
			want: "Init:ErrImagePull", ready: "0/2", unhealthy: true},
		"init running": {status: corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{State: running}}},
			want: "Init:0/1", ready: "0/2"},
		"creating": {status: corev1.PodStatus{Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{State: done}},
			ContainerStatuses:     []corev1.ContainerStatus{{State: waiting("ContainerCreating")}, {State: waiting("ContainerCreating")}}},
			want: "ContainerCreating", ready: "0/2", unhealthy: true},
		"native sidecar running": {status: corev1.PodStatus{Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "side", Started: &started, Ready: true, State: running, RestartCount: 3}},
			ContainerStatuses:     []corev1.ContainerStatus{{Ready: true, State: running}, {Ready: true, State: running}}},
			spec: &sidecarSpec, want: "Running", ready: "3/3", restarts: 3},
		"native sidecar main crashloop": {status: corev1.PodStatus{Phase: corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "side", Started: &started, Ready: true, State: running}},
			ContainerStatuses:     []corev1.ContainerStatus{{Ready: true, State: running}, {State: waiting("CrashLoopBackOff")}}},
			spec: &sidecarSpec, want: "CrashLoopBackOff", ready: "2/3", unhealthy: true},
		"evicted":     {status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}, want: "Evicted", ready: "0/2"},
		"terminating": {status: corev1.PodStatus{Phase: corev1.PodRunning}, deleting: true, want: "Terminating", ready: "0/2"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := two
			if tc.spec != nil {
				spec = *tc.spec
			}
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", CreationTimestamp: created}, Spec: spec, Status: tc.status}
			if tc.deleting {
				p.DeletionTimestamp = &created
			}
			r := PodRowOf(p, now)
			if r.Status != tc.want || r.Ready != tc.ready || Unhealthy(r) != tc.unhealthy {
				t.Fatalf("got %+v unhealthy=%v", r, Unhealthy(r))
			}
			if tc.restarts != 0 && r.Restarts != tc.restarts {
				t.Fatalf("restarts %d, want %d", r.Restarts, tc.restarts)
			}
			if r.Age != "90s" {
				t.Fatalf("age %q", r.Age)
			}
		})
	}
}

func TestObjectsSkipsEmptyDocuments(t *testing.T) {
	objs, err := Objects([]byte("---\n# Source: x\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\n\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"))
	if err != nil || len(objs) != 2 || objs[1].GetName() != "b" {
		t.Fatalf("%v %v", objs, err)
	}
	if _, err := Objects([]byte("kind: ConfigMap\n")); err == nil {
		t.Fatal("a document without apiVersion must fail")
	}
}
