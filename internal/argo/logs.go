package argo

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
)

const (
	defaultLogTail = 200
	maxLogTail     = 1000
	// maxLogPods is how many pods, the newest, a read of every pod covers.
	maxLogPods = 10
	// maxContainerLog is the most one container's lines take; maxLogBytes
	// the most of all of them, the newest kept.
	maxContainerLog = 1 << 20
	maxLogBytes     = 2 << 20
)

// Logs reads the last lines of an app's pods: of one pod, or of every pod
// merged by time. Only pods in the app's tree can be read.
func (w *Watcher) Logs(ctx context.Context, req protocol.AppLogsRequest) (*protocol.AppLogs, error) {
	_, objs, err := w.appTree(req.App)
	if err != nil {
		return nil, err
	}
	var pods []*corev1.Pod
	for _, k := range objs {
		if k.Kind != "Pod" {
			continue
		}
		if p := w.objects.pod(k.Namespace, k.Name); p != nil {
			pods = append(pods, p)
		}
	}
	slices.SortFunc(pods, func(a, b *corev1.Pod) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	targets, err := logTargets(pods, req)
	if err != nil {
		return nil, err
	}

	tail := int64(req.TailLines)
	if tail <= 0 {
		tail = defaultLogTail
	}
	tail = min(tail, maxLogTail)
	limit := int64(maxContainerLog)
	read := make([][]protocol.LogLine, len(targets))
	failed := make([]string, len(targets))
	cut := make([]bool, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := w.k.Kube.CoreV1().Pods(t.pod.Namespace).GetLogs(t.pod.Name, &corev1.PodLogOptions{
				Container: t.container, Previous: req.Previous, Timestamps: true, TailLines: &tail, LimitBytes: &limit,
			}).DoRaw(ctx)
			if err != nil {
				// What the API server says, such as a container waiting to start.
				failed[i] = kube.LogsError(err, raw).Error()
				return
			}
			read[i], cut[i] = parseLog(t.pod.Name, t.container, raw, len(raw) >= maxContainerLog)
		}()
	}
	wg.Wait()

	out := &protocol.AppLogs{Pods: make([]protocol.AppPod, 0, len(pods)), Lines: []protocol.LogLine{}}
	for _, p := range pods {
		out.Pods = append(out.Pods, appPod(p))
	}
	for i, t := range targets {
		if failed[i] != "" {
			out.Errors = append(out.Errors, protocol.LogError{Pod: t.pod.Name, Container: t.container, Message: failed[i]})
		}
		out.Lines = append(out.Lines, read[i]...)
		out.Truncated = out.Truncated || cut[i]
	}
	var dropped bool
	out.Lines, dropped = mergeLog(out.Lines)
	out.Truncated = out.Truncated || dropped
	return out, nil
}

type logTarget struct {
	pod       *corev1.Pod
	container string
}

// logTargets picks the containers to read: of the pod asked for, or of the
// newest pods. A container left empty is each pod's first. Reading the
// previous run of every pod skips the pods whose container never restarted.
func logTargets(pods []*corev1.Pod, req protocol.AppLogsRequest) ([]logTarget, error) {
	pick := func(p *corev1.Pod) string {
		if req.Container != "" || len(p.Spec.Containers) == 0 {
			return req.Container
		}
		return p.Spec.Containers[0].Name
	}
	if req.Pod != "" {
		i := slices.IndexFunc(pods, func(p *corev1.Pod) bool { return p.Name == req.Pod })
		if i < 0 {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "pod %s is not a pod of app %s", req.Pod, req.App)
		}
		p, c := pods[i], pick(pods[i])
		if !slices.Contains(containerNames(p.Spec.Containers), c) && !slices.Contains(containerNames(p.Spec.InitContainers), c) {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "pod %s has no container %q", p.Name, c)
		}
		return []logTarget{{p, c}}, nil
	}
	out := []logTarget{}
	for _, p := range pods {
		if len(out) == maxLogPods {
			break
		}
		c := pick(p)
		if !slices.Contains(containerNames(p.Spec.Containers), c) {
			continue
		}
		if req.Previous && restarts(p, c) == 0 {
			continue
		}
		out = append(out, logTarget{p, c})
	}
	return out, nil
}

// parseLog splits what the kubelet returns with timestamps into lines. A
// line without a timestamp takes that of the line before. When the read hit
// its size limit, the last line is cut short and is dropped.
func parseLog(pod, container string, raw []byte, hitLimit bool) ([]protocol.LogLine, bool) {
	text := string(raw)
	if hitLimit {
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			text = text[:i]
		}
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, hitLimit
	}
	var (
		lines []protocol.LogLine
		at    time.Time
	)
	for _, line := range strings.Split(text, "\n") {
		ts, rest, ok := strings.Cut(line, " ")
		if t, err := time.Parse(time.RFC3339Nano, ts); ok && err == nil {
			at, line = t.UTC(), rest
		}
		lines = append(lines, protocol.LogLine{Pod: pod, Container: container, Time: at, Text: strings.TrimSuffix(line, "\r")})
	}
	return lines, hitLimit
}

// mergeLog orders the lines of several containers by time, each
// container's in its own order, and keeps the newest that fit in
// maxLogBytes.
func mergeLog(lines []protocol.LogLine) ([]protocol.LogLine, bool) {
	slices.SortStableFunc(lines, func(a, b protocol.LogLine) int { return a.Time.Compare(b.Time) })
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		size += len(lines[i].Text) + 1
		if size > maxLogBytes {
			return lines[i+1:], true
		}
	}
	return lines, false
}

// appPod is what the app page shows of a pod.
func appPod(p *corev1.Pod) protocol.AppPod {
	ap := protocol.AppPod{
		Name:           p.Name,
		Status:         podStatus(p),
		Containers:     containerNames(p.Spec.Containers),
		InitContainers: containerNames(p.Spec.InitContainers),
		Node:           p.Spec.NodeName,
		Created:        p.CreationTimestamp.UTC(),
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ap.Ready = true
		}
	}
	for _, c := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		ap.Restarts += int(c.RestartCount)
	}
	return ap
}

func containerNames(cs []corev1.Container) []string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return names
}

func restarts(p *corev1.Pod, container string) int32 {
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == container {
			return c.RestartCount
		}
	}
	return 0
}

// podStatus is the STATUS column of kubectl get pods: the phase, or what
// holds the pod's init containers or containers.
func podStatus(p *corev1.Pod) string {
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	initializing := false
	for i, c := range p.Status.InitContainerStatuses {
		switch {
		case c.State.Terminated != nil && c.State.Terminated.ExitCode == 0:
			continue
		case c.State.Terminated != nil && c.State.Terminated.Reason != "":
			reason = "Init:" + c.State.Terminated.Reason
		case c.State.Terminated != nil:
			reason = fmt.Sprintf("Init:ExitCode:%d", c.State.Terminated.ExitCode)
		case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + c.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}
	if !initializing {
		running := false
		for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
			c := p.Status.ContainerStatuses[i]
			switch {
			case c.State.Waiting != nil && c.State.Waiting.Reason != "":
				reason = c.State.Waiting.Reason
			case c.State.Terminated != nil && c.State.Terminated.Reason != "":
				reason = c.State.Terminated.Reason
			case c.State.Terminated != nil && c.State.Terminated.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", c.State.Terminated.Signal)
			case c.State.Terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", c.State.Terminated.ExitCode)
			case c.Ready && c.State.Running != nil:
				running = true
			}
		}
		// A container that finished next to one that runs.
		if reason == "Completed" && running {
			reason = "Running"
		}
	}
	if p.DeletionTimestamp != nil {
		reason = "Terminating"
	}
	return reason
}
