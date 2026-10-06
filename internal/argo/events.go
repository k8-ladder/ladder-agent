package argo

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// maxAppEvents is the most events app.events returns, the newest.
const maxAppEvents = 200

// appTree returns the Application of the app and the objects of its tree.
func (w *Watcher) appTree(name string) (objKey, []objKey, error) {
	w.mu.RLock()
	a, ok := w.apps[name]
	watching := w.watching
	w.mu.RUnlock()
	if !watching {
		return objKey{}, nil, protocol.Errorf(protocol.CodeFailedPrecondition, "the agent does not watch Argo CD yet")
	}
	if !ok {
		return objKey{}, nil, protocol.Errorf(protocol.CodeFailedPrecondition, "Argo CD has no Application %s yet", name)
	}
	// Set before watching started.
	return objKey{w.namespace, "Application", name}, tree(w.objects, a.top), nil
}

// Events reads the events of an app: those of its Application, such as a
// sync or a change of health, and those of the objects in its tree, Normal
// and Warning, newest first.
func (w *Watcher) Events(ctx context.Context, name string) (*protocol.AppEvents, error) {
	application, objs, err := w.appTree(name)
	if err != nil {
		return nil, err
	}
	// The watcher keeps only Warning events: read them all from the API.
	var namespaces []string
	for _, k := range objs {
		if k.Namespace != "" && !slices.Contains(namespaces, k.Namespace) {
			namespaces = append(namespaces, k.Namespace)
		}
	}
	var events []corev1.Event
	for _, ns := range namespaces {
		list, err := w.k.Kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		events = append(events, list.Items...)
	}
	sel := fields.Set{"involvedObject.kind": application.Kind, "involvedObject.name": application.Name}
	list, err := w.k.Kube.CoreV1().Events(application.Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel.AsSelector().String()})
	if err != nil {
		return nil, err
	}
	events = append(events, list.Items...)
	return appEvents(append(objs, application), events), nil
}

// podSuffix is what a ReplicaSet, Job or DaemonSet adds to its own name to
// name a pod.
var podSuffix = regexp.MustCompile(`^-[a-z0-9]{5}$`)

// appEvents keeps the events about objs, newest first. A pod that left the
// tree, such as one an earlier rollout deleted, still counts by its name: that
// of a ReplicaSet, Job or DaemonSet of the tree and the suffix it adds.
func appEvents(objs []objKey, events []corev1.Event) *protocol.AppEvents {
	in := map[objKey]bool{}
	var makers []objKey
	for _, k := range objs {
		in[k] = true
		if k.Kind == "ReplicaSet" || k.Kind == "Job" || k.Kind == "DaemonSet" {
			makers = append(makers, k)
		}
	}
	ours := func(k objKey) bool {
		if in[k] {
			return true
		}
		if k.Kind != "Pod" {
			return false
		}
		for _, m := range makers {
			if rest, ok := strings.CutPrefix(k.Name, m.Name); ok && m.Namespace == k.Namespace && podSuffix.MatchString(rest) {
				return true
			}
		}
		return false
	}
	out := &protocol.AppEvents{Events: []protocol.AppEvent{}}
	for i := range events {
		e := &events[i]
		k := objKey{e.InvolvedObject.Namespace, e.InvolvedObject.Kind, e.InvolvedObject.Name}
		if !ours(k) {
			continue
		}
		first, last := eventTimes(e)
		count := int(e.Count)
		if e.Series != nil {
			count = max(count, int(e.Series.Count))
		}
		out.Events = append(out.Events, protocol.AppEvent{
			Type: e.Type, Reason: e.Reason, Kind: k.Kind, Name: k.Name,
			Message: strings.TrimSpace(e.Message), Count: max(count, 1),
			FirstSeen: first.UTC(), LastSeen: last.UTC(),
		})
	}
	slices.SortStableFunc(out.Events, func(a, b protocol.AppEvent) int { return b.LastSeen.Compare(a.LastSeen) })
	if len(out.Events) > maxAppEvents {
		out.Events, out.Truncated = out.Events[:maxAppEvents], true
	}
	return out
}
