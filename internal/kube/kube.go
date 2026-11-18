// Package kube is the client-go side of captain_utils: one client built from
// the operator's kubeconfig and the small reads the flows need.
package kube

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps a clientset, plus a dynamic client and REST mapper for applying
// arbitrary manifests (nil in tests that do not apply).
type Client struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Mapper  meta.RESTMapper
}

// RESTConfig loads the kubeconfig the way kubectl does (KUBECONFIG, then
// ~/.kube/config), optionally switching to kubeContext.
func RESTConfig(kubeContext, userAgent string) (*rest.Config, error) {
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg.UserAgent = userAgent
	cfg.Timeout = 30 * time.Second
	return cfg, nil
}

// New builds a Client for kubeContext ("" = current context).
func New(kubeContext, userAgent string) (*Client, error) {
	cfg, err := RESTConfig(kubeContext, userAgent)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))
	return &Client{Kube: cs, Dynamic: dyn, Mapper: mapper}, nil
}

var minorVersion = regexp.MustCompile(`^v?(\d+\.\d+)`)

// ServerVersion is the API server's major.minor ("1.34"), read from its git
// version so EKS's "34+" minor does not leak in.
func (c *Client) ServerVersion(context.Context) (string, error) {
	v, err := c.Kube.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("server version: %w", err)
	}
	m := minorVersion.FindStringSubmatch(v.GitVersion)
	if m == nil {
		return "", fmt.Errorf("server version: cannot parse %q", v.GitVersion)
	}
	return m[1], nil
}

// DeploymentLabel returns a label of a Deployment ("" when the Deployment or label is absent).
func (c *Client) DeploymentLabel(ctx context.Context, namespace, name, key string) (string, error) {
	d, err := c.Kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return d.Labels[key], nil
}

// DeploymentImage returns the image of a Deployment's first container ("" when absent).
func (c *Client) DeploymentImage(ctx context.Context, namespace, name string) (string, error) {
	d, err := c.Kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if cs := d.Spec.Template.Spec.Containers; len(cs) > 0 {
		return cs[0].Image, nil
	}
	return "", nil
}

// ReleaseChartVersion returns the chart version of the newest revision of a
// Helm release that `helm list -A` would show (status deployed or failed), in
// any namespace. It reads Helm's own storage (the sh.helm.release.v1 Secrets),
// so it agrees with the helm CLI without needing it. "" when there is no such
// release.
func (c *Client) ReleaseChartVersion(ctx context.Context, release string) (string, error) {
	list, err := c.Kube.CoreV1().Secrets("").List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + release})
	if err != nil {
		return "", fmt.Errorf("list helm releases: %w", err)
	}
	type rev struct {
		n    int
		data []byte
	}
	var revs []rev
	for _, s := range list.Items {
		if st := s.Labels["status"]; st != "deployed" && st != "failed" {
			continue
		}
		n, _ := strconv.Atoi(s.Labels["version"])
		revs = append(revs, rev{n, s.Data["release"]})
	}
	if len(revs) == 0 {
		return "", nil
	}
	sort.Slice(revs, func(i, j int) bool { return revs[i].n > revs[j].n })
	r, err := DecodeRelease(revs[0].data)
	if err != nil {
		return "", fmt.Errorf("helm release %s: %w", release, err)
	}
	return r.Chart.Metadata.Version, nil
}

// Release is the part of a stored Helm release captain_utils reads.
type Release struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Chart     struct {
		Metadata struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
}

var gzipMagic = []byte{0x1f, 0x8b, 0x08}

// DecodeRelease decodes a Helm storage record: base64, then gzip (when the
// magic bytes say so), then JSON.
func DecodeRelease(data []byte) (Release, error) {
	var r Release
	b := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	n, err := base64.StdEncoding.Decode(b, data)
	if err != nil {
		return r, fmt.Errorf("base64: %w", err)
	}
	b = b[:n]
	if bytes.HasPrefix(b, gzipMagic) {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return r, err
		}
		if b, err = io.ReadAll(zr); err != nil {
			return r, err
		}
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("json: %w", err)
	}
	return r, nil
}
