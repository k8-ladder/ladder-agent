package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Poll calls cond every interval until it returns true, an error, or ctx ends.
func Poll(ctx context.Context, interval time.Duration, cond func() (bool, error)) error {
	return wait.PollUntilContextCancel(ctx, interval, true, func(context.Context) (bool, error) { return cond() })
}

// WorkloadsReady reports whether every Deployment and StatefulSet in namespace
// (or only the named Deployments) has rolled out. The second value lists what
// is not ready yet.
func WorkloadsReady(ctx context.Context, k *Clients, namespace string, deployments ...string) (bool, []string, error) {
	var notReady []string
	deps, err := k.Kube.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, nil, err
	}
	want := map[string]bool{}
	for _, d := range deployments {
		want[d] = true
	}
	found := 0
	for _, d := range deps.Items {
		if len(want) > 0 && !want[d.Name] {
			continue
		}
		found++
		if !DeploymentReady(&d) {
			notReady = append(notReady, "deployment/"+d.Name)
		}
	}
	if len(want) > 0 && found < len(want) {
		return false, []string{"deployments not created yet"}, nil
	}
	if len(want) == 0 {
		sts, err := k.Kube.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, nil, err
		}
		for _, s := range sts.Items {
			if !StatefulSetReady(&s) {
				notReady = append(notReady, "statefulset/"+s.Name)
			}
		}
		if len(deps.Items)+len(sts.Items) == 0 {
			return false, []string{"no workloads yet"}, nil
		}
	}
	return len(notReady) == 0, notReady, nil
}

// WaitDeploymentsReady blocks until WorkloadsReady holds or ctx ends.
func WaitDeploymentsReady(ctx context.Context, k *Clients, namespace string, deployments ...string) error {
	return Poll(ctx, 3*time.Second, func() (bool, error) {
		ok, _, err := WorkloadsReady(ctx, k, namespace, deployments...)
		return ok, err
	})
}

// DeploymentReady reports whether d has rolled out all its replicas.
func DeploymentReady(d *appsv1.Deployment) bool {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas >= want &&
		d.Status.AvailableReplicas >= want
}

// StatefulSetReady reports whether s has all its replicas ready.
func StatefulSetReady(s *appsv1.StatefulSet) bool {
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	return s.Status.ObservedGeneration >= s.Generation && s.Status.ReadyReplicas >= want
}

// Diagnose summarises unhealthy pods and recent warning events in namespace,
// for the error message of a timed-out step.
func Diagnose(ctx context.Context, k *Clients, namespace string) string {
	var lines []string
	pods, err := k.Kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, p := range pods.Items {
			if line := podProblem(&p); line != "" {
				lines = append(lines, line)
			}
		}
	}
	events, err := k.Kube.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err == nil {
		sort.Slice(events.Items, func(i, j int) bool {
			return events.Items[i].LastTimestamp.After(events.Items[j].LastTimestamp.Time)
		})
		for i, e := range events.Items {
			if i == 5 {
				break
			}
			lines = append(lines, fmt.Sprintf("event %s/%s: %s: %s", strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name, e.Reason, e.Message))
		}
	}
	return strings.Join(lines, "\n")
}

func podProblem(p *corev1.Pod) string {
	if p.Status.Phase == corev1.PodSucceeded {
		return ""
	}
	for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" {
			return fmt.Sprintf("pod %s: container %s %s: %s", p.Name, cs.Name, w.Reason, w.Message)
		}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue {
			return fmt.Sprintf("pod %s: %s, not ready", p.Name, p.Status.Phase)
		}
	}
	return ""
}
