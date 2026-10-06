package argo

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAppEvents(t *testing.T) {
	application := objKey{"argocd", "Application", "web"}
	deploy := objKey{"dev", "Deployment", "web"}
	rs := objKey{"dev", "ReplicaSet", "web-7d9f8"}
	pod := objKey{"dev", "Pod", "web-7d9f8-x2kqp"}
	job := objKey{"dev", "Job", "web-migrate"}
	objs := []objKey{application, deploy, rs, pod, job}

	event := func(k objKey, typ, reason string, last time.Duration) corev1.Event {
		e := *kubeletEvent(reason, "  "+reason+" message ", last-time.Minute, last)
		e.InvolvedObject = corev1.ObjectReference{Namespace: k.Namespace, Kind: k.Kind, Name: k.Name}
		e.Type, e.Count = typ, 1
		return e
	}
	series := event(pod, "Warning", "BackOff", 0)
	series.Count, series.FirstTimestamp, series.LastTimestamp = 0, metav1.Time{}, metav1.Time{}
	series.EventTime = metav1.NewMicroTime(t0)
	series.Series = &corev1.EventSeries{Count: 7, LastObservedTime: metav1.NewMicroTime(t0.Add(9 * time.Minute))}

	got := appEvents(objs, []corev1.Event{
		event(deploy, "Normal", "ScalingReplicaSet", 2*time.Minute),
		event(application, "Normal", "OperationCompleted", 3*time.Minute),
		series,
		// Pods an earlier rollout or a finished Job left behind, by name.
		event(objKey{"dev", "Pod", "web-7d9f8-abcde"}, "Normal", "Killing", time.Minute),
		event(objKey{"dev", "Pod", "web-migrate-q8z2n"}, "Normal", "Started", 4*time.Minute),
		// Not the app's: another app's objects, a longer name, another namespace.
		event(objKey{"dev", "Deployment", "web-api"}, "Normal", "ScalingReplicaSet", 5*time.Minute),
		event(objKey{"dev", "Pod", "web-7d9f8-abcde-1"}, "Normal", "Killing", 5*time.Minute),
		event(objKey{"prod", "Pod", "web-7d9f8-abcde"}, "Normal", "Killing", 5*time.Minute),
		event(objKey{"argocd", "Application", "web-api"}, "Normal", "OperationCompleted", 5*time.Minute),
	})

	want := []struct {
		reason, kind, name string
		count              int
	}{
		{"BackOff", "Pod", "web-7d9f8-x2kqp", 7},
		{"Started", "Pod", "web-migrate-q8z2n", 1},
		{"OperationCompleted", "Application", "web", 1},
		{"ScalingReplicaSet", "Deployment", "web", 1},
		{"Killing", "Pod", "web-7d9f8-abcde", 1},
	}
	if len(got.Events) != len(want) || got.Truncated {
		t.Fatalf("got %d events (truncated %v), want %d: %+v", len(got.Events), got.Truncated, len(want), got.Events)
	}
	for i, w := range want {
		e := got.Events[i]
		if e.Reason != w.reason || e.Kind != w.kind || e.Name != w.name || e.Count != w.count {
			t.Errorf("event %d = %s %s/%s x%d, want %s %s/%s x%d", i, e.Reason, e.Kind, e.Name, e.Count, w.reason, w.kind, w.name, w.count)
		}
	}
	first := got.Events[0]
	if first.Type != "Warning" || first.Message != "BackOff message" || !first.FirstSeen.Equal(t0) || !first.LastSeen.Equal(t0.Add(9*time.Minute)) {
		t.Errorf("series event = %+v", first)
	}

	var many []corev1.Event
	for i := range maxAppEvents + 5 {
		many = append(many, event(deploy, "Normal", "ScalingReplicaSet", time.Duration(i)*time.Second))
	}
	if got := appEvents(objs, many); len(got.Events) != maxAppEvents || !got.Truncated || !got.Events[0].LastSeen.Equal(t0.Add(time.Duration(maxAppEvents+4)*time.Second)) {
		t.Errorf("of %d events: %d kept, truncated %v, newest %v", len(many), len(got.Events), got.Truncated, got.Events[0].LastSeen)
	}
}
