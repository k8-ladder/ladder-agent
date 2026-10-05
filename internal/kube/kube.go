// Package kube builds Kubernetes clients and small helpers shared by the agent.
package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// FieldManager is the server-side apply field manager of the agent.
const FieldManager = "ladder-agent"

var (
	ApplicationGVR    = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	ApplicationSetGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applicationsets"}
)

type Clients struct {
	Config  *rest.Config
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Mapper  *restmapper.DeferredDiscoveryRESTMapper
	// Context is the kubeconfig context name, or "in-cluster".
	Context string
}

// New loads kubeconfig the way kubectl does and falls back to the in-cluster
// config.
func New() (*Clients, error) {
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kube config: %w", err)
	}
	contextName := "in-cluster"
	if raw, err := loader.RawConfig(); err == nil && raw.CurrentContext != "" {
		contextName = raw.CurrentContext
	}
	cfg.UserAgent = "ladder-agent"

	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(kc.Discovery()))
	return &Clients{Config: cfg, Kube: kc, Dynamic: dc, Mapper: mapper, Context: contextName}, nil
}

// HasResource reports whether the API server serves the resource, using
// discovery (allowed for every authenticated user).
func (c *Clients) HasResource(gvr schema.GroupVersionResource) (bool, error) {
	list, err := c.Kube.Discovery().ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, r := range list.APIResources {
		if r.Name == gvr.Resource {
			return true, nil
		}
	}
	return false, nil
}

// ApplyYAML server-side applies a single YAML document.
func (c *Clients) ApplyYAML(ctx context.Context, doc []byte) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(doc, &obj.Object); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return c.Apply(ctx, obj)
}

// Apply server-side applies obj, taking ownership of conflicting fields.
func (c *Clients) Apply(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		c.Mapper.Reset()
		mapping, err = c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("map %s: %w", gvk, err)
	}
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ri = c.Dynamic.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	}
	return ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
}

// EnsureNamespace creates the namespace if it does not exist.
func (c *Clients) EnsureNamespace(ctx context.Context, name string) error {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)
	_, err := c.Apply(ctx, ns)
	return err
}

// EnsureNamespaceLabels creates the namespace if needed and sets labels on
// it. Other labels stay.
func (c *Clients) EnsureNamespaceLabels(ctx context.Context, name string, labels map[string]string) error {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)
	ns.SetLabels(labels)
	_, err := c.Apply(ctx, ns)
	return err
}

// NamespaceHasLabels reports whether the namespace exists and has all labels.
func (c *Clients) NamespaceHasLabels(ctx context.Context, name string, labels map[string]string) (bool, error) {
	ns, err := c.Kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for k, v := range labels {
		if ns.Labels[k] != v {
			return false, nil
		}
	}
	return true, nil
}

// UpsertSecret creates the Secret or replaces its data and labels.
func (c *Clients) UpsertSecret(ctx context.Context, s *corev1.Secret) error {
	secrets := c.Kube.CoreV1().Secrets(s.Namespace)
	cur, err := secrets.Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, s, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	cur.Labels = s.Labels
	cur.Data = s.Data
	cur.StringData = nil
	_, err = secrets.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}
