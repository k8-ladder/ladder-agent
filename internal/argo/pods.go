package argo

import (
	"reflect"

	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
)

// appPods counts the pods in the tree under top that run or start, and how
// many of them are ready. Pods that finished, such as a Job's, and pods being
// deleted, such as those of a rollout's old ReplicaSet, do not count.
func appPods(o objects, top []objKey) *protocol.Pods {
	n := &protocol.Pods{}
	for _, k := range tree(o, top) {
		if k.Kind != "Pod" {
			continue
		}
		p := o.pod(k.Namespace, k.Name)
		if p == nil || p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		n.Total++
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				n.Ready++
			}
		}
	}
	return n
}

// sameStatus compares statuses by value, pods and Ingresses included.
func sameStatus(a, b protocol.AppStatus) bool {
	return reflect.DeepEqual(a, b)
}
