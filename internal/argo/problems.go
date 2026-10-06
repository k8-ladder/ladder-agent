package argo

import (
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/cache"
)

// An app's problems are what Kubernetes reports as wrong with its objects,
// whatever their kind: the Warning events of every object in the app's tree.
// Kubernetes reports each kind of failure as an event: a pod no node has room
// for, a ReplicaSet a quota stops, a volume that does not mount, a failing
// probe, an image that does not pull. The tree is what Argo CD manages for the
// app (status.resources) and what those objects own, down through the objects
// the built-in controllers create: ReplicaSets, Jobs and Pods.

// ownedKinds are the kinds searched for children, by owner reference.
var ownedKinds = []string{"ReplicaSet", "Job", "Pod"}

const (
	maxProblemLines = 10
	// eventSlack reaches back before the app left Healthy: the events that
	// made it leave may come a moment earlier.
	eventSlack = time.Minute

	ownerIndex    = "owner"
	involvedIndex = "involved"
)

// objKey names an object; Namespace is empty for cluster-scoped ones.
type objKey struct{ Namespace, Kind, Name string }

func (k objKey) String() string { return k.Namespace + "/" + k.Kind + "/" + k.Name }

// objects reads the watched objects.
type objects interface {
	// children returns the objects that k owns.
	children(k objKey) []objKey
	// events returns the Warning events about k.
	events(k objKey) []*corev1.Event
	// pod returns a pod, or nil.
	pod(namespace, name string) *corev1.Pod
	// ingress returns an Ingress, or nil.
	ingress(namespace, name string) *networkingv1.Ingress
}

// appProblems lists the problems in the tree under top seen since since, one
// per line, oldest first: the first line is usually the cause. A problem that
// several objects of a kind share takes one line.
func appProblems(o objects, top []objKey, since time.Time) string {
	type group struct {
		kind, text string
		names      []string
		first      time.Time
	}
	groups := map[string]*group{}
	add := func(k objKey, text string, at time.Time) {
		id := k.Kind + "\x00" + text
		g := groups[id]
		if g == nil {
			g = &group{kind: k.Kind, text: text, first: at}
			groups[id] = g
		}
		if !slices.Contains(g.names, k.Name) {
			g.names = append(g.names, k.Name)
		}
		if at.Before(g.first) {
			g.first = at
		}
	}
	for _, k := range tree(o, top) {
		for _, e := range o.events(k) {
			first, last := eventTimes(e)
			if !last.Before(since) {
				add(k, e.Reason+": "+strings.TrimSpace(e.Message), first)
			}
		}
		if k.Kind == "Pod" {
			if p := o.pod(k.Namespace, k.Name); p != nil {
				for _, c := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
					if text, at := lastExit(c); text != "" && !at.Before(since) {
						add(k, text, at)
					}
				}
			}
		}
	}

	sorted := make([]*group, 0, len(groups))
	for _, g := range groups {
		sorted = append(sorted, g)
	}
	slices.SortFunc(sorted, func(a, b *group) int {
		if c := a.first.Compare(b.first); c != 0 {
			return c
		}
		return strings.Compare(a.kind+a.text, b.kind+b.text)
	})
	var lines []string
	for _, g := range sorted {
		if len(lines) == maxProblemLines {
			lines = append(lines, fmt.Sprintf("and %d more", len(sorted)-maxProblemLines))
			break
		}
		slices.Sort(g.names)
		names := strings.Join(g.names, ", ")
		if len(g.names) > 3 {
			names = fmt.Sprintf("%s and %d more", strings.Join(g.names[:3], ", "), len(g.names)-3)
		}
		lines = append(lines, g.kind+" "+names+": "+g.text)
	}
	return strings.Join(lines, "\n")
}

// tree returns top and everything it owns.
func tree(o objects, top []objKey) []objKey {
	seen := map[objKey]bool{}
	var out []objKey
	queue := slices.Clone(top)
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
		queue = append(queue, o.children(k)...)
	}
	return out
}

// eventTimes reads both event styles: kubelet's (first and last timestamp)
// and that of events.k8s.io reporters such as the scheduler (event time and
// series).
func eventTimes(e *corev1.Event) (first, last time.Time) {
	last = e.LastTimestamp.Time
	if t := e.EventTime.Time; t.After(last) {
		last = t
	}
	if e.Series != nil && e.Series.LastObservedTime.After(last) {
		last = e.Series.LastObservedTime.Time
	}
	first = e.FirstTimestamp.Time
	if first.IsZero() {
		first = e.EventTime.Time
	}
	if first.IsZero() {
		first = last
	}
	return first, last
}

// lastExit says how a container that is waiting to restart last ended, which
// its back-off event does not: "container app exited: OOMKilled, code 137".
func lastExit(c corev1.ContainerStatus) (string, time.Time) {
	t := c.LastTerminationState.Terminated
	if c.State.Waiting == nil || t == nil || t.ExitCode == 0 {
		return "", time.Time{}
	}
	reason := t.Reason
	if reason == "" {
		reason = "Error"
	}
	return fmt.Sprintf("container %s exited: %s, code %d", c.Name, reason, t.ExitCode), t.FinishedAt.Time
}

// indexes is objects over informer caches.
type indexes struct {
	owned     map[string]cache.Indexer // by kind, indexed by owner
	warned    cache.Indexer            // Warning events, indexed by involved object
	ingresses cache.Indexer
}

func (x *indexes) children(k objKey) []objKey {
	var out []objKey
	for _, kind := range ownedKinds {
		objs, _ := x.owned[kind].ByIndex(ownerIndex, k.String())
		for _, obj := range objs {
			if m, err := meta.Accessor(obj); err == nil {
				out = append(out, objKey{m.GetNamespace(), kind, m.GetName()})
			}
		}
	}
	return out
}

func (x *indexes) events(k objKey) []*corev1.Event {
	objs, _ := x.warned.ByIndex(involvedIndex, k.String())
	out := make([]*corev1.Event, 0, len(objs))
	for _, obj := range objs {
		if e, ok := obj.(*corev1.Event); ok {
			out = append(out, e)
		}
	}
	return out
}

func (x *indexes) pod(namespace, name string) *corev1.Pod {
	obj, ok, _ := x.owned["Pod"].GetByKey(namespace + "/" + name)
	if !ok {
		return nil
	}
	p, _ := obj.(*corev1.Pod)
	return p
}

func (x *indexes) ingress(namespace, name string) *networkingv1.Ingress {
	obj, ok, _ := x.ingresses.GetByKey(namespace + "/" + name)
	if !ok {
		return nil
	}
	ing, _ := obj.(*networkingv1.Ingress)
	return ing
}

// byOwner indexes an object under each of its owners.
func byOwner(obj any) ([]string, error) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, nil
	}
	var keys []string
	for _, r := range m.GetOwnerReferences() {
		keys = append(keys, objKey{m.GetNamespace(), r.Kind, r.Name}.String())
	}
	return keys, nil
}

// byInvolved indexes an event under the object it is about.
func byInvolved(obj any) ([]string, error) {
	e, ok := obj.(*corev1.Event)
	if !ok {
		return nil, nil
	}
	return []string{objKey{e.InvolvedObject.Namespace, e.InvolvedObject.Kind, e.InvolvedObject.Name}.String()}, nil
}
