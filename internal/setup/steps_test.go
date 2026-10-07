package setup

import (
	"context"
	"strings"
	"testing"

	"github.com/ladder/ladder-agent/internal/kube"
	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

// argocdResources is status.resources of Application argocd as Argo CD 3
// writes it: without the health of each resource.
const argocdResources = `
resourceHealthSource: appTree
resources:
- {group: apps, kind: Deployment, namespace: argocd, name: argocd-server, status: Synced}
- {group: apps, kind: StatefulSet, namespace: argocd, name: argocd-application-controller, status: Synced}
- {group: networking.k8s.io, kind: Ingress, namespace: argocd, name: argocd-server, status: Synced}
- {kind: ConfigMap, namespace: argocd, name: argocd-cm, status: Synced}`

func TestAppSynced(t *testing.T) {
	one := int32(1)
	deploy := func(available int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "argocd-server"},
			Spec:       appsv1.DeploymentSpec{Replicas: &one},
			Status:     appsv1.DeploymentStatus{UpdatedReplicas: 1, AvailableReplicas: available},
		}
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "argocd-application-controller"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	ingress := func(ip string) *networkingv1.Ingress {
		ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "argocd", Name: "argocd-server"}}
		if ip != "" {
			ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: ip}}
		}
		return ing
	}
	cases := []struct {
		name, status string
		objs         []runtime.Object
		ok           bool
		line         string
	}{
		{"healthy", "sync: {status: Synced}\nhealth: {status: Healthy}", nil, true, "Synced, Healthy"},
		{"ingress without address", "sync: {status: Synced}\nhealth: {status: Progressing}\noperationState: {message: successfully synced (all tasks run)}" + argocdResources,
			[]runtime.Object{deploy(1), sts, ingress("")}, true,
			"Synced, Progressing: successfully synced (all tasks run); Ingress argocd/argocd-server has no address (no ingress controller in the cluster?)"},
		{"deployment rolling out", "sync: {status: Synced}\nhealth: {status: Progressing}" + argocdResources,
			[]runtime.Object{deploy(0), sts, ingress("")}, false,
			"Synced, Progressing; Deployment argocd/argocd-server is not ready; Ingress argocd/argocd-server has no address"},
		{"statefulset not created", "sync: {status: Synced}\nhealth: {status: Progressing}" + argocdResources,
			[]runtime.Object{deploy(1), ingress("")}, false,
			"Synced, Progressing; StatefulSet argocd/argocd-application-controller is not ready"},
		{"ingress with address", "sync: {status: Synced}\nhealth: {status: Progressing}" + argocdResources,
			[]runtime.Object{deploy(1), sts, ingress("10.0.0.1")}, false, "Synced, Progressing"},
		{"out of sync", "sync: {status: OutOfSync}\nhealth: {status: Progressing}" + argocdResources,
			[]runtime.Object{deploy(1), sts, ingress("")}, false, "OutOfSync, Progressing"},
		{"degraded", "sync: {status: Synced}\nhealth: {status: Degraded}" + argocdResources,
			[]runtime.Object{deploy(1), sts, ingress("")}, false, "Synced, Degraded"},
		{"no status", "", nil, false, "Unknown, Unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var status map[string]any
			if err := yaml.Unmarshal([]byte(c.status), &status); err != nil {
				t.Fatal(err)
			}
			u := &unstructured.Unstructured{Object: map[string]any{"status": status}}
			k := &kube.Clients{Kube: fake.NewClientset(c.objs...)}
			ok, line, err := appSynced(context.Background(), k, u)
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.ok {
				t.Errorf("ok = %v, want %v (%s)", ok, c.ok, line)
			}
			if !strings.HasPrefix(line, c.line) {
				t.Errorf("line = %q, want prefix %q", line, c.line)
			}
		})
	}
}
