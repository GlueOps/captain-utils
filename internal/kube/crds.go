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
	"k8s.io/apimachinery/pkg/util/wait"
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
		if ctx.Err() == nil && wait.Interrupted(err) {
			return fmt.Errorf("timed out after %s: %s", timeout, strings.Join(pending, " "))
		}
		return err
	}
	return nil
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
