package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Objects splits a multi-document YAML (or JSON) stream into objects, skipping
// empty documents (helm template --include-crds emits one before each CRD).
func Objects(data []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for i := 1; ; i++ {
		var m map[string]interface{}
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.GetKind() == "" || u.GetAPIVersion() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("document %d: missing apiVersion, kind or metadata.name", i)
		}
		out = append(out, u)
	}
}

// ApplyOptions are a server-side apply's knobs.
type ApplyOptions struct {
	FieldManager string
	Force        bool // --force-conflicts
	DryRun       bool // --dry-run=server
}

// Apply server-side-applies obj, like `kubectl apply --server-side`, and
// returns the object the API server answered with.
func (c *Client) Apply(ctx context.Context, obj *unstructured.Unstructured, o ApplyOptions) (*unstructured.Unstructured, error) {
	if c.Dynamic == nil || c.Mapper == nil {
		return nil, errors.New("kube client has no dynamic client")
	}
	gvk := obj.GroupVersionKind()
	m, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
	}
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}
	force := o.Force
	po := metav1.PatchOptions{FieldManager: o.FieldManager, Force: &force}
	if o.DryRun {
		po.DryRun = []string{metav1.DryRunAll}
	}
	ri := c.Dynamic.Resource(m.Resource)
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		return ri.Namespace(ns).Patch(ctx, obj.GetName(), types.ApplyPatchType, data, po)
	}
	return ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, po)
}

// DeleteDaemonSet deletes a DaemonSet; it reports false when there was none.
func (c *Client) DeleteDaemonSet(ctx context.Context, namespace, name string) (bool, error) {
	err := c.Kube.AppsV1().DaemonSets(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
