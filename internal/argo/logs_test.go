package argo

import (
	"strings"
	"testing"
	"time"

	"github.com/k8-ladder/ladder-agent/protocol"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodStatus(t *testing.T) {
	waiting := func(reason string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
	}
	ended := func(reason string, code int32) corev1.ContainerState {
		return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}}
	}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	pod := func(phase corev1.PodPhase, inits []corev1.ContainerState, states ...corev1.ContainerState) *corev1.Pod {
		p := &corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
		for _, s := range inits {
			p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{})
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, corev1.ContainerStatus{State: s})
		}
		for _, s := range states {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{State: s, Ready: s.Running != nil})
		}
		return p
	}
	terminating := pod(corev1.PodRunning, nil, running)
	terminating.DeletionTimestamp = &metav1.Time{Time: t0}
	evicted := pod(corev1.PodFailed, nil)
	evicted.Status.Reason = "Evicted"

	for name, tt := range map[string]struct {
		pod  *corev1.Pod
		want string
	}{
		"running":            {pod(corev1.PodRunning, nil, running), "Running"},
		"not scheduled":      {pod(corev1.PodPending, nil), "Pending"},
		"crashing":           {pod(corev1.PodRunning, nil, waiting("CrashLoopBackOff")), "CrashLoopBackOff"},
		"bad image":          {pod(corev1.PodPending, nil, waiting("ImagePullBackOff")), "ImagePullBackOff"},
		"job done":           {pod(corev1.PodSucceeded, nil, ended("Completed", 0)), "Completed"},
		"killed":             {pod(corev1.PodRunning, nil, ended("", 137)), "ExitCode:137"},
		"sidecar finished":   {pod(corev1.PodRunning, nil, running, ended("Completed", 0)), "Running"},
		"init runs":          {pod(corev1.PodPending, []corev1.ContainerState{ended("Completed", 0), running}, waiting("PodInitializing")), "Init:1/2"},
		"init fails":         {pod(corev1.PodPending, []corev1.ContainerState{ended("Error", 1)}, waiting("PodInitializing")), "Init:Error"},
		"init crash loops":   {pod(corev1.PodPending, []corev1.ContainerState{waiting("CrashLoopBackOff")}), "Init:CrashLoopBackOff"},
		"being deleted":      {terminating, "Terminating"},
		"evicted by kubelet": {evicted, "Evicted"},
	} {
		if got := podStatus(tt.pod); got != tt.want {
			t.Errorf("%s: got %q, want %q", name, got, tt.want)
		}
	}
}

func TestLogTargets(t *testing.T) {
	pod := func(name string, restarts int32, containers ...string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		for _, c := range containers {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: c, RestartCount: restarts})
		}
		p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
		return p
	}
	pods := []*corev1.Pod{pod("web-2", 0, "web", "proxy"), pod("web-1", 3, "web"), pod("worker-1", 1, "worker")}
	names := func(ts []logTarget) string {
		var out []string
		for _, t := range ts {
			out = append(out, t.pod.Name+"/"+t.container)
		}
		return strings.Join(out, " ")
	}

	for name, tt := range map[string]struct {
		req  protocol.AppLogsRequest
		want string
		err  string
	}{
		"every pod's first container": {req: protocol.AppLogsRequest{}, want: "web-2/web web-1/web worker-1/worker"},
		"a container some pods have":  {req: protocol.AppLogsRequest{Container: "proxy"}, want: "web-2/proxy"},
		"previous runs of restarted":  {req: protocol.AppLogsRequest{Previous: true}, want: "web-1/web worker-1/worker"},
		"one pod":                     {req: protocol.AppLogsRequest{Pod: "web-2"}, want: "web-2/web"},
		"one pod's init container":    {req: protocol.AppLogsRequest{Pod: "web-2", Container: "migrate"}, want: "web-2/migrate"},
		"a pod of another app":        {req: protocol.AppLogsRequest{App: "web", Pod: "db-0"}, err: "pod db-0 is not a pod of app web"},
		"a container the pod lacks":   {req: protocol.AppLogsRequest{Pod: "web-1", Container: "proxy"}, err: `pod web-1 has no container "proxy"`},
	} {
		got, err := logTargets(pods, tt.req)
		switch {
		case tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)):
			t.Errorf("%s: err = %v, want %q", name, err, tt.err)
		case tt.err == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tt.err == "" && names(got) != tt.want:
			t.Errorf("%s: got %q, want %q", name, names(got), tt.want)
		}
	}

	var many []*corev1.Pod
	for range maxLogPods + 2 {
		many = append(many, pod("web", 0, "web"))
	}
	if got, _ := logTargets(many, protocol.AppLogsRequest{}); len(got) != maxLogPods {
		t.Errorf("of %d pods, %d read", len(many), len(got))
	}
}

func TestParseAndMergeLog(t *testing.T) {
	a, _ := parseLog("web-1", "web", []byte(
		"2026-10-05T12:30:00.000000001Z listening on :8080\n"+
			"2026-10-05T12:30:02Z GET / 200\r\n"+
			"continued without a timestamp\n"), false)
	b, _ := parseLog("web-2", "web", []byte("2026-10-05T12:30:01Z listening on :8080\n2026-10-05T12:30:02Z GET /health 200\n"), false)
	lines, cut := mergeLog(append(a, b...))
	var got []string
	for _, l := range lines {
		got = append(got, l.Pod+" "+l.Time.Format("05.000")+" "+l.Text)
	}
	want := []string{
		"web-1 00.000 listening on :8080",
		"web-2 01.000 listening on :8080",
		"web-1 02.000 GET / 200",
		"web-1 02.000 continued without a timestamp",
		"web-2 02.000 GET /health 200",
	}
	if cut || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("cut %v, got\n%s\nwant\n%s", cut, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if lines, cut := parseLog("web-1", "web", []byte(""), false); len(lines) != 0 || cut {
		t.Errorf("empty log: %v %v", lines, cut)
	}
	// A read that hit its size limit ends inside a line.
	lines, cut = parseLog("web-1", "web", []byte("2026-10-05T12:30:00Z whole\n2026-10-05T12:30:01Z hal"), true)
	if len(lines) != 1 || lines[0].Text != "whole" || !cut {
		t.Errorf("cut read: %+v %v", lines, cut)
	}

	long := strings.Repeat("x", maxLogBytes/4)
	var big []protocol.LogLine
	for i := range 6 {
		big = append(big, protocol.LogLine{Time: t0.Add(time.Duration(i) * time.Second), Text: long})
	}
	if kept, cut := mergeLog(big); len(kept) != 3 || !cut || !kept[2].Time.Equal(t0.Add(5*time.Second)) {
		t.Errorf("kept %d of %d lines, cut %v", len(kept), len(big), cut)
	}
}
