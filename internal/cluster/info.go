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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	if req, limits, err := i.planned(ctx); err != nil {
		warn("pod requests", err)
	} else {
		info.Requested = req
		info.Limits = &limits
		info.Free = protocol.Resources{
			CPUMillis:   max(info.Allocatable.CPUMillis-req.CPUMillis, 0),
			MemoryBytes: max(info.Allocatable.MemoryBytes-req.MemoryBytes, 0),
		}
	}

	// Metrics Server is optional. It gives the resource panel a live reading
	// when present, while clusters without it still report capacity and plans.
	if available, err := i.k.HasResource(kube.PodMetricsGVR); err != nil {
		warn("pod usage", err)
	} else if available {
		used, err := i.usage(ctx)
		if err != nil {
			warn("pod usage", err)
		} else {
			info.Used = &used
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
	out.Addresses, out.Hosts, out.TLSHosts = i.ingress(ctx, d.Namespace)
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

// ingress reads Ingress argocd-server: the addresses the ingress controller
// reported for it, the hosts of its rules, and those it serves over HTTPS
// with a certificate in place. None without the Ingress.
func (i *Inspector) ingress(ctx context.Context, namespace string) (addresses, hosts, tlsHosts []string) {
	ing, err := i.k.Kube.NetworkingV1().Ingresses(namespace).Get(ctx, "argocd-server", metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil
	}
	hasCert := func(name string) bool {
		s, err := i.k.Kube.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		return err == nil && len(s.Data[corev1.TLSCertKey]) > 0
	}
	return kube.IngressAddresses(ing), kube.IngressHosts(ing), kube.IngressTLSHosts(ing, hasCert)
}

// planned sums container requests and limits of all pods that occupy a node.
// These are the resources the pod specs plan for, including the platform pods
// that use the cluster alongside applications.
func (i *Inspector) planned(ctx context.Context) (requests, limits protocol.Resources, err error) {
	pods, err := i.k.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=Succeeded,status.phase!=Failed",
	})
	if err != nil {
		return requests, limits, err
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" {
			continue
		}
		for _, c := range p.Spec.Containers {
			req := resources(c.Resources.Requests)
			requests.CPUMillis += req.CPUMillis
			requests.MemoryBytes += req.MemoryBytes
			limit := resources(c.Resources.Limits)
			limits.CPUMillis += limit.CPUMillis
			limits.MemoryBytes += limit.MemoryBytes
		}
	}
	return requests, limits, nil
}

// usage sums the resource usage Metrics Server reports for running pod
// containers. PodMetrics is an aggregated API, so it is read through the
// dynamic client rather than client-go's core Kubernetes client.
func (i *Inspector) usage(ctx context.Context) (protocol.Resources, error) {
	var out protocol.Resources
	metrics, err := i.k.Dynamic.Resource(kube.PodMetricsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, err
	}
	for _, pod := range metrics.Items {
		containers, found, err := unstructured.NestedSlice(pod.Object, "containers")
		if err != nil {
			return out, fmt.Errorf("read containers in pod metric %s/%s: %w", pod.GetNamespace(), pod.GetName(), err)
		}
		if !found {
			continue
		}
		for _, container := range containers {
			values, ok := container.(map[string]any)
			if !ok {
				return out, fmt.Errorf("read container in pod metric %s/%s", pod.GetNamespace(), pod.GetName())
			}
			usage, found, err := unstructured.NestedStringMap(values, "usage")
			if err != nil {
				return out, fmt.Errorf("read usage in pod metric %s/%s: %w", pod.GetNamespace(), pod.GetName(), err)
			}
			if !found {
				continue
			}
			used, err := metricResources(usage)
			if err != nil {
				return out, fmt.Errorf("read usage in pod metric %s/%s: %w", pod.GetNamespace(), pod.GetName(), err)
			}
			out.CPUMillis += used.CPUMillis
			out.MemoryBytes += used.MemoryBytes
		}
	}
	return out, nil
}

func metricResources(values map[string]string) (protocol.Resources, error) {
	cpu, ok := values[string(corev1.ResourceCPU)]
	if !ok {
		return protocol.Resources{}, fmt.Errorf("no CPU value")
	}
	memory, ok := values[string(corev1.ResourceMemory)]
	if !ok {
		return protocol.Resources{}, fmt.Errorf("no memory value")
	}
	cpuQuantity, err := resource.ParseQuantity(cpu)
	if err != nil {
		return protocol.Resources{}, fmt.Errorf("parse CPU %q: %w", cpu, err)
	}
	memoryQuantity, err := resource.ParseQuantity(memory)
	if err != nil {
		return protocol.Resources{}, fmt.Errorf("parse memory %q: %w", memory, err)
	}
	return protocol.Resources{CPUMillis: cpuQuantity.MilliValue(), MemoryBytes: memoryQuantity.Value()}, nil
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
