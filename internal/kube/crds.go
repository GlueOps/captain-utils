package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/csaupgrade"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

func (c *Client) crds() (interface {
	List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error)
	Get(context.Context, string, metav1.GetOptions, ...string) (*unstructured.Unstructured, error)
	Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions, ...string) (*unstructured.Unstructured, error)
}, error) {
	if c.Dynamic == nil {
		return nil, errors.New("kube client has no dynamic client")
	}
	return c.Dynamic.Resource(crdGVR), nil
}

// ListCRDs returns every CRD on the cluster, managedFields included, sorted by name.
func (c *Client) ListCRDs(ctx context.Context) ([]*unstructured.Unstructured, error) {
	ri, err := c.crds()
	if err != nil {
		return nil, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list CRDs: %w", err)
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out, nil
}

// RemoveCRDAnnotation removes one annotation from a CRD (a merge patch; an absent key is fine).
func (c *Client) RemoveCRDAnnotation(ctx context.Context, name, key string) error {
	return c.removeCRDMetadata(ctx, name, "annotations", key)
}

// RemoveCRDLabel removes one label from a CRD.
func (c *Client) RemoveCRDLabel(ctx context.Context, name, key string) error {
	return c.removeCRDMetadata(ctx, name, "labels", key)
}

func (c *Client) removeCRDMetadata(ctx context.Context, name, field, key string) error {
	ri, err := c.crds()
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"metadata":{%q:{%q:null}}}`, field, key)
	if _, err := ri.Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("remove %s %s from crd %s: %w", field, key, name, err)
	}
	return nil
}

func (c *Client) pollCRDs(ctx context.Context, names []string, timeout time.Duration, done func(*unstructured.Unstructured, error) (bool, error)) error {
	ri, err := c.crds()
	if err != nil {
		return err
	}
	pending := append([]string(nil), names...)
	start := time.Now()
	err = wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		var still []string
		for _, n := range pending {
			u, err := ri.Get(ctx, n, metav1.GetOptions{})
			ok, err := done(u, err)
			if err != nil {
				return false, err
			}
			if !ok {
				still = append(still, n)
			}
		}
		pending = still
		return len(pending) == 0, nil
	})
	if err != nil {
		// the deadline can also expire inside client-go's rate limiter ("rate: Wait(n=1) would exceed
		// context deadline"), which wait.Interrupted does not recognise: that is still a timeout
		if ctx.Err() == nil && (wait.Interrupted(err) || time.Since(start) >= timeout) {
			return &TimeoutError{Timeout: timeout, Pending: pending}
		}
		return err
	}
	return nil
}

// TimeoutError is a wait that ran out of time; Pending are the CRDs it was still waiting for.
type TimeoutError struct {
	Timeout time.Duration
	Pending []string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s: %s", e.Timeout, strings.Join(e.Pending, " "))
}

// WaitCRDsDeleted waits until none of the CRDs exist (kubectl wait --for=delete).
func (c *Client) WaitCRDsDeleted(ctx context.Context, names []string, timeout time.Duration) error {
	return c.pollCRDs(ctx, names, timeout, func(_ *unstructured.Unstructured, err error) (bool, error) {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// WaitCRDsEstablished waits until every CRD has condition Established=True
// (kubectl wait --for=condition=Established).
func (c *Client) WaitCRDsEstablished(ctx context.Context, names []string, timeout time.Duration) error {
	return c.pollCRDs(ctx, names, timeout, func(u *unstructured.Unstructured, err error) (bool, error) {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, cd := range conds {
			m, _ := cd.(map[string]interface{})
			if m["type"] == "Established" && m["status"] == "True" {
				return true, nil
			}
		}
		return false, nil
	})
}

// lastApplied is the annotation client-side apply keeps its own record in.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

var lastAppliedPath = fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "annotations", lastApplied))

// MigrateCRDToSSA is the step `kubectl apply --server-side` takes before it
// applies: a CRD that was managed by client-side apply (an Update owner of the
// last-applied annotation, normally kubectl-client-side-apply) has that
// ownership handed to manager as an Apply owner. The server-side apply that
// follows then drops the annotation and the old manager, because manager no
// longer sets them. It reports whether anything was migrated.
func (c *Client) MigrateCRDToSSA(ctx context.Context, name, manager string) (bool, error) {
	ri, err := c.crds()
	if err != nil {
		return false, err
	}
	for range 5 { // conflicts on resourceVersion: re-read and retry, as kubectl does
		obj, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil // not installed yet (a fresh cluster): nothing to migrate
		}
		if err != nil {
			return false, fmt.Errorf("get crd %s: %w", name, err)
		}
		managers := sets.New[string]()
		for _, e := range csaupgrade.FindFieldsOwners(obj.GetManagedFields(), metav1.ManagedFieldsOperationUpdate, lastAppliedPath) {
			managers.Insert(e.Manager)
		}
		patch, err := csaupgrade.UpgradeManagedFieldsPatch(obj, managers, manager)
		if err != nil {
			return false, fmt.Errorf("migrate crd %s to server-side apply: %w", name, err)
		}
		if patch == nil {
			return false, nil
		}
		if _, err = ri.Patch(ctx, name, types.JSONPatchType, patch, metav1.PatchOptions{}); err == nil {
			return true, nil
		} else if !apierrors.IsConflict(err) {
			return false, fmt.Errorf("migrate crd %s to server-side apply: %w", name, err)
		}
	}
	return false, fmt.Errorf("migrate crd %s to server-side apply: too many conflicts", name)
}
