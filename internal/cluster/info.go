// Package cluster collects facts about the cluster for the cluster screen
// and the setup check.
package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"

type Inspector struct {
	k *kube.Clients
}

func NewInspector(k *kube.Clients) *Inspector { return &Inspector{k: k} }

// Info never fails on missing permissions: what cannot be read goes to Warnings.
func (i *Inspector) Info(ctx context.Context) (*protocol.ClusterInfo, error) {
	info := &protocol.ClusterInfo{Context: i.k.Context, Nodes: []protocol.Node{}, StorageClasses: []protocol.StorageClass{}}
	warn := func(what string, err error) {
		info.Warnings = append(info.Warnings, fmt.Sprintf("%s: %v", what, err))
	}

	v, err := i.k.Kube.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("server version: %w", err)
	}
	info.KubernetesVersion = v.GitVersion

	nodes, err := i.k.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		warn("nodes", err)
	} else {
		for _, n := range nodes.Items {
			alloc := resources(n.Status.Allocatable)
			info.Allocatable.CPUMillis += alloc.CPUMillis
			info.Allocatable.MemoryBytes += alloc.MemoryBytes
			info.Nodes = append(info.Nodes, protocol.Node{
				Name:           n.Name,
				Ready:          nodeReady(&n),
				Roles:          nodeRoles(&n),
				KubeletVersion: n.Status.NodeInfo.KubeletVersion,
				Allocatable:    alloc,
			})
		}
	}

	if req, err := i.requested(ctx); err != nil {
		warn("pod requests", err)
	} else {
		info.Requested = req
		info.Free = protocol.Resources{
			CPUMillis:   max(info.Allocatable.CPUMillis-req.CPUMillis, 0),
			MemoryBytes: max(info.Allocatable.MemoryBytes-req.MemoryBytes, 0),
		}
	}

	scs, err := i.k.Kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		warn("storage classes", err)
	} else {
		for _, sc := range scs.Items {
			info.StorageClasses = append(info.StorageClasses, protocol.StorageClass{
				Name:        sc.Name,
				Provisioner: sc.Provisioner,
				Default:     sc.Annotations[defaultClassAnnotation] == "true",
			})
		}
	}

	if ok, err := i.SetupAccess(ctx); err != nil {
		warn("setup access", err)
	} else {
		info.SetupAccess = ok
	}

	if argo, err := i.ArgoCD(ctx); err != nil {
		warn("argo cd detection", err)
	} else {
		info.ArgoCD = *argo
	}
	return info, nil
}

// SetupAccess reports whether the agent can do anything anywhere, which is
// what installing CRDs and Argo CD needs.
func (i *Inspector) SetupAccess(ctx context.Context) (bool, error) {
	r, err := i.k.Kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{Verb: "*", Group: "*", Resource: "*"},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return r.Status.Allowed, nil
}

// ArgoCD detects Argo CD: the Application CRD exists and a Deployment
// argocd-server runs in some namespace.
func (i *Inspector) ArgoCD(ctx context.Context) (*protocol.ArgoCDInfo, error) {
	crd, err := i.k.HasResource(kube.ApplicationGVR)
	if err != nil {
		return nil, err
	}
	out := &protocol.ArgoCDInfo{CRD: crd}
	if !crd {
		return out, nil
	}
	deps, err := i.k.Kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=argocd-server"})
	if err != nil {
		return nil, err
	}
	if len(deps.Items) == 0 {
		return out, nil
	}
	d := deps.Items[0]
	out.Found = true
	out.Namespace = d.Namespace
	out.Addresses = i.ingressAddresses(ctx, d.Namespace)
	if len(out.Addresses) > 0 {
		out.Address = out.Addresses[0]
	}
	out.Version = d.Labels["app.kubernetes.io/version"]
	if out.Version == "" && len(d.Spec.Template.Spec.Containers) > 0 {
		img := d.Spec.Template.Spec.Containers[0].Image
		if j := strings.LastIndex(img, ":"); j > strings.LastIndex(img, "/") {
			out.Version = img[j+1:]
		}
	}
	return out, nil
}

// ingressAddresses are the addresses the ingress controller reported for
// Ingress argocd-server, none while there are none.
func (i *Inspector) ingressAddresses(ctx context.Context, namespace string) []string {
	ing, err := i.k.Kube.NetworkingV1().Ingresses(namespace).Get(ctx, "argocd-server", metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return kube.IngressAddresses(ing)
}

// HasDefaultStorageClass reports whether PVCs without a class get a volume.
func (i *Inspector) HasDefaultStorageClass(ctx context.Context) (bool, error) {
	scs, err := i.k.Kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, sc := range scs.Items {
		if sc.Annotations[defaultClassAnnotation] == "true" {
			return true, nil
		}
	}
	return false, nil
}

// requested sums container requests of all pods that occupy a node.
func (i *Inspector) requested(ctx context.Context) (protocol.Resources, error) {
	var out protocol.Resources
	pods, err := i.k.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=Succeeded,status.phase!=Failed",
	})
	if err != nil {
		return out, err
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" {
			continue
		}
		for _, c := range p.Spec.Containers {
			r := resources(c.Resources.Requests)
			out.CPUMillis += r.CPUMillis
			out.MemoryBytes += r.MemoryBytes
		}
	}
	return out, nil
}

func resources(l corev1.ResourceList) protocol.Resources {
	return protocol.Resources{CPUMillis: l.Cpu().MilliValue(), MemoryBytes: l.Memory().Value()}
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeRoles(n *corev1.Node) []string {
	roles := []string{}
	for k := range n.Labels {
		if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && r != "" {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	return roles
}
