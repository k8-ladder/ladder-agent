package argo

import (
	"strings"
	"testing"
	"time"

	"github.com/k8-ladder/ladder-agent/protocol"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// fakeObjects is a cluster of owner links, events, pods and Ingresses.
type fakeObjects struct {
	owns   map[objKey][]objKey
	warned map[objKey][]*corev1.Event
	pods   map[string]*corev1.Pod
	ings   map[string]*networkingv1.Ingress
}

func (f *fakeObjects) children(k objKey) []objKey      { return f.owns[k] }
func (f *fakeObjects) events(k objKey) []*corev1.Event { return f.warned[k] }
func (f *fakeObjects) pod(ns, name string) *corev1.Pod { return f.pods[ns+"/"+name] }
func (f *fakeObjects) ingress(ns, name string) *networkingv1.Ingress {
	return f.ings[ns+"/"+name]
}
func (f *fakeObjects) warn(k objKey, e *corev1.Event) { f.warned[k] = append(f.warned[k], e) }
func newFakeObjects() *fakeObjects {
	return &fakeObjects{owns: map[objKey][]objKey{}, warned: map[objKey][]*corev1.Event{}, pods: map[string]*corev1.Pod{},
		ings: map[string]*networkingv1.Ingress{}}
}

var t0 = time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC)

// kubeletEvent has first and last timestamps, as kubelet and controllers write them.
func kubeletEvent(reason, msg string, first, last time.Duration) *corev1.Event {
	return &corev1.Event{Reason: reason, Message: msg,
		FirstTimestamp: metav1.NewTime(t0.Add(first)), LastTimestamp: metav1.NewTime(t0.Add(last))}
}

// schedulerEvent has an event time and a series, as events.k8s.io reporters write them.
func schedulerEvent(reason, msg string, at, seen time.Duration) *corev1.Event {
	e := &corev1.Event{Reason: reason, Message: msg, EventTime: metav1.NewMicroTime(t0.Add(at))}
	if seen > at {
		e.Series = &corev1.EventSeries{Count: 2, LastObservedTime: metav1.NewMicroTime(t0.Add(seen))}
	}
	return e
}

func TestAppProblems(t *testing.T) {
	const noCPU = "0/1 nodes are available: 1 Insufficient cpu."
	deploy := objKey{"dev", "Deployment", "web"}
	svc := objKey{"dev", "Service", "web"}
	rsOld, rsNew := objKey{"dev", "ReplicaSet", "web-old"}, objKey{"dev", "ReplicaSet", "web-new"}
	pod := func(name string) objKey { return objKey{"dev", "Pod", name} }

	cluster := func() *fakeObjects {
		f := newFakeObjects()
		f.owns[deploy] = []objKey{rsOld, rsNew}
		f.owns[rsOld] = []objKey{pod("web-old-1")}
		f.owns[rsNew] = []objKey{pod("web-new-1"), pod("web-new-2")}
		return f
	}
	top := []objKey{deploy, svc}

	t.Run("nothing wrong", func(t *testing.T) {
		if got := appProblems(cluster(), top, time.Time{}); got != "" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("pods of one rollout share a line; other kinds and objects outside the tree count or not", func(t *testing.T) {
		f := cluster()
		f.warn(pod("web-new-1"), schedulerEvent("FailedScheduling", noCPU, 0, 5*time.Minute))
		f.warn(pod("web-new-2"), schedulerEvent("FailedScheduling", noCPU, time.Second, 0))
		f.warn(rsNew, kubeletEvent("FailedCreate", `pods "web-new-3" is forbidden: exceeded quota: compute`, 2*time.Second, 2*time.Second))
		f.warn(svc, kubeletEvent("FailedToUpdateEndpoint", "Failed to update endpoint dev/web", 3*time.Second, 3*time.Second))
		f.warn(objKey{"dev", "Pod", "other-app-1"}, kubeletEvent("BackOff", "not ours", 0, 0))
		want := strings.Join([]string{
			"Pod web-new-1, web-new-2: FailedScheduling: " + noCPU,
			`ReplicaSet web-new: FailedCreate: pods "web-new-3" is forbidden: exceeded quota: compute`,
			"Service web: FailedToUpdateEndpoint: Failed to update endpoint dev/web",
		}, "\n")
		if got := appProblems(f, top, time.Time{}); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("events last seen before the window are over", func(t *testing.T) {
		f := cluster()
		f.warn(pod("web-old-1"), kubeletEvent("Unhealthy", "Readiness probe failed", 0, time.Minute))
		f.warn(pod("web-new-1"), schedulerEvent("FailedScheduling", noCPU, 0, 10*time.Minute))
		want := "Pod web-new-1: FailedScheduling: " + noCPU
		if got := appProblems(f, top, t0.Add(5*time.Minute)); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("a crashing container says how it exited", func(t *testing.T) {
		f := cluster()
		f.warn(pod("web-new-1"), kubeletEvent("BackOff", "Back-off restarting failed container app", time.Minute, 2*time.Minute))
		f.pods["dev/web-new-1"] = &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 "app",
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.NewTime(t0)}},
		}}}}
		want := "Pod web-new-1: container app exited: OOMKilled, code 137\nPod web-new-1: BackOff: Back-off restarting failed container app"
		if got := appProblems(f, top, time.Time{}); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("many objects and lines are cut", func(t *testing.T) {
		f := cluster()
		for i, name := range []string{"a", "b", "c", "d", "e"} {
			f.owns[rsNew] = append(f.owns[rsNew], pod(name))
			f.warn(pod(name), kubeletEvent("Unhealthy", "Liveness probe failed", time.Duration(i), time.Duration(i)))
		}
		for i := range maxProblemLines + 2 {
			f.warn(rsNew, kubeletEvent("FailedCreate", strings.Repeat("x", i+1), time.Duration(10+i)*time.Second, time.Hour))
		}
		lines := strings.Split(appProblems(f, top, time.Time{}), "\n")
		if lines[0] != "Pod a, b, c and 2 more: Unhealthy: Liveness probe failed" {
			t.Errorf("first line %q", lines[0])
		}
		if len(lines) != maxProblemLines+1 || lines[maxProblemLines] != "and 3 more" {
			t.Errorf("lines:\n%s", strings.Join(lines, "\n"))
		}
	})
}

func TestProblemsWindow(t *testing.T) {
	const noCPU = "0/1 nodes are available: 1 Insufficient cpu."
	deploy, rs, pod := objKey{"dev", "Deployment", "web"}, objKey{"dev", "ReplicaSet", "web-1"}, objKey{"dev", "Pod", "web-1-a"}
	f := newFakeObjects()
	f.owns[deploy], f.owns[rs] = []objKey{rs}, []objKey{pod}
	w := &Watcher{apps: map[string]app{}, objects: f, watching: true}
	var changes []protocol.AppStatus
	w.OnChange = func(c protocol.AppsChanged) { changes = append(changes, c.App) }
	application := func(health string, since time.Time) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "web"},
			"spec":     map[string]any{"destination": map[string]any{"namespace": "dev"}},
			"status": map[string]any{
				"health":    map[string]any{"status": health, "lastTransitionTime": since.Format(time.RFC3339)},
				"resources": []any{map[string]any{"kind": "Deployment", "namespace": "dev", "name": "web"}},
			},
		}}
	}
	problems := func() string { return w.apps["web"].status.Problems }

	// A probe failed during an earlier rollout, which then finished.
	f.warn(pod, kubeletEvent("Unhealthy", "Readiness probe failed", 0, time.Minute))
	w.upsert(application("Healthy", t0.Add(2*time.Minute)))
	if problems() != "" {
		t.Errorf("Healthy app has problems: %q", problems())
	}

	// The next rollout is stuck: only its own events count.
	f.warn(pod, schedulerEvent("FailedScheduling", noCPU, 30*time.Minute, 0))
	w.upsert(application("Progressing", t0.Add(30*time.Minute)))
	want := "Pod web-1-a: FailedScheduling: " + noCPU
	if problems() != want {
		t.Errorf("Progressing: %q, want %q", problems(), want)
	}
	// Ten minutes later Argo CD gives up; the window stays where the rollout began.
	w.upsert(application("Degraded", t0.Add(40*time.Minute)))
	if problems() != want {
		t.Errorf("Degraded: %q, want %q", problems(), want)
	}

	// An event arriving later updates the app without an Application change.
	f.warn(rs, kubeletEvent("FailedCreate", "exceeded quota", 41*time.Minute, 41*time.Minute))
	n := len(changes)
	w.objectChanged(&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "dev"}})
	if len(changes) != n+1 || !strings.Contains(changes[n].Problems, "exceeded quota") {
		t.Errorf("after a new event: %+v", changes[n:])
	}
	w.objectChanged(&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "other"}})
	if len(changes) != n+1 {
		t.Errorf("an event in another namespace changed the app")
	}

	w.upsert(application("Healthy", t0.Add(50*time.Minute)))
	if problems() != "" {
		t.Errorf("Healthy again: %q", problems())
	}
}
