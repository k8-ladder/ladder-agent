package argo

import (
	"testing"

	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAppPods(t *testing.T) {
	deploy, job := objKey{"dev", "Deployment", "web"}, objKey{"dev", "Job", "migrate"}
	rsOld, rsNew := objKey{"dev", "ReplicaSet", "web-old"}, objKey{"dev", "ReplicaSet", "web-new"}
	f := newFakeObjects()
	f.owns[deploy] = []objKey{rsOld, rsNew}
	f.owns[rsOld] = []objKey{{"dev", "Pod", "web-old-1"}}
	f.owns[rsNew] = []objKey{{"dev", "Pod", "web-new-1"}, {"dev", "Pod", "web-new-2"}, {"dev", "Pod", "web-new-3"}}
	f.owns[job] = []objKey{{"dev", "Pod", "migrate-1"}}
	f.owns[objKey{"dev", "Deployment", "other"}] = []objKey{{"dev", "Pod", "other-1"}}
	pod := func(name string, phase corev1.PodPhase, ready bool) {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "dev", Name: name}, Status: corev1.PodStatus{Phase: phase}}
		if ready {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		f.pods["dev/"+name] = p
	}
	pod("web-old-1", corev1.PodRunning, true)
	f.pods["dev/web-old-1"].DeletionTimestamp = &metav1.Time{}
	pod("web-new-1", corev1.PodRunning, true)
	pod("web-new-2", corev1.PodRunning, false)
	pod("web-new-3", corev1.PodPending, false)
	pod("migrate-1", corev1.PodSucceeded, false)
	pod("other-1", corev1.PodRunning, true)

	if got := appPods(f, []objKey{deploy, job}); *got != (protocol.Pods{Ready: 1, Total: 3}) {
		t.Errorf("pods = %+v, want 1 of 3 ready", *got)
	}
	if got := appPods(f, nil); *got != (protocol.Pods{}) {
		t.Errorf("pods of an app without objects = %+v", *got)
	}
}

func TestSameStatus(t *testing.T) {
	a := protocol.AppStatus{Name: "web", Health: "Healthy", Pods: &protocol.Pods{Ready: 2, Total: 2}}
	b := a
	b.Pods = &protocol.Pods{Ready: 2, Total: 2}
	if !sameStatus(a, b) {
		t.Error("equal pods in other pointers differ")
	}
	b.Pods = &protocol.Pods{Ready: 1, Total: 2}
	if sameStatus(a, b) {
		t.Error("other pods are the same")
	}
	b.Pods = nil
	if sameStatus(a, b) {
		t.Error("no pods is the same as pods")
	}
	b = a
	b.Health = "Degraded"
	if sameStatus(a, b) {
		t.Error("other health is the same")
	}
}
