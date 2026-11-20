package crds

import (
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Pure readers over live objects: no cluster calls, no mutations. The plan
// block, the guards, the post-apply assertions and check all go through these,
// so what the operator is shown cannot drift from what the guards act on.

// terminating names the CRDs that are being deleted.
func terminating(live []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range live {
		if o.GetDeletionTimestamp() != nil {
			out = append(out, o.GetName())
		}
	}
	return out
}

// argoTracked names the CRDs carrying ArgoCD tracking metadata.
func argoTrackedAnnotation(live []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range live {
		if _, ok := o.GetAnnotations()[argoTrackingID]; ok {
			out = append(out, o.GetName())
		}
	}
	return out
}

func argoTrackedLabel(live []*unstructured.Unstructured) []string {
	var out []string
	for _, o := range live {
		if _, ok := o.GetLabels()[argoInstance]; ok {
			out = append(out, o.GetName())
		}
	}
	return out
}

// unowned names the CRDs no apply by manager has touched.
func unowned(live []*unstructured.Unstructured, manager string) []string {
	var out []string
	for _, o := range live {
		owned := false
		for _, f := range o.GetManagedFields() {
			if f.Manager == manager && f.Operation == "Apply" {
				owned = true
			}
		}
		if !owned {
			out = append(out, o.GetName())
		}
	}
	return out
}

// storedVersions is the storedVersions check: a stored API version the new
// bundle no longer declares makes the API server reject the update ("must
// remain in spec.versions until a storage migration"). Errors block; warnings
// are shown.
func storedVersions(b *bundle, live []*unstructured.Unstructured) (errs, warns []string) {
	type vs struct{ all, served map[string]bool }
	in := map[string]vs{}
	for _, o := range b.objs {
		v := vs{map[string]bool{}, map[string]bool{}}
		list, _, _ := unstructured.NestedSlice(o.Object, "spec", "versions")
		for _, e := range list {
			m, _ := e.(map[string]interface{})
			n, _ := m["name"].(string)
			v.all[n] = true
			if s, _ := m["served"].(bool); s {
				v.served[n] = true
			}
		}
		in[o.GetName()] = v
	}
	for _, o := range live {
		v, ok := in[o.GetName()]
		if !ok {
			continue
		}
		stored, _, _ := unstructured.NestedStringSlice(o.Object, "status", "storedVersions")
		for _, sv := range stored {
			switch {
			case !v.all[sv]:
				errs = append(errs, fmt.Sprintf("%s stores %s, which platform-crds no longer defines", o.GetName(), sv))
			case !v.served[sv]:
				warns = append(warns, fmt.Sprintf("%s stores %s, which is no longer served — migrate storage before the next bump", o.GetName(), sv))
			}
		}
	}
	return errs, warns
}

// diffResult is what an apply would change.
type diffResult struct {
	New, Changed int
	Text         string
}

func (d diffResult) drift() bool { return d.New+d.Changed > 0 }

// normalize is what is compared and shown: the object without managedFields.
func normalize(o *unstructured.Unstructured) string {
	c := o.DeepCopy()
	unstructured.RemoveNestedField(c.Object, "metadata", "managedFields")
	b, err := yaml.Marshal(c.Object)
	if err != nil {
		return fmt.Sprintf("%v", c.Object)
	}
	return string(b)
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return c
}

func join(s []string) string { return strings.Join(sortedCopy(s), " ") }
