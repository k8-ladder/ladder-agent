package upgrade

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/k8-ladder/ladder-agent/internal/kube"
	"github.com/k8-ladder/ladder-agent/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRepository(t *testing.T) {
	for in, want := range map[string]string{
		"ladder-agent:dev":                         "ladder-agent",
		"ghcr.io/ladder/agent:1.2.0":               "ghcr.io/ladder/agent",
		"registry.local:5000/ladder/agent:1.2.0":   "registry.local:5000/ladder/agent",
		"registry.local:5000/ladder/agent":         "registry.local:5000/ladder/agent",
		"ghcr.io/ladder/agent:1.2.0@sha256:abc123": "ghcr.io/ladder/agent",
	} {
		if got := repository(in); got != want {
			t.Errorf("repository(%q) = %q, want %q", in, got, want)
		}
	}
}

func newTestUpgrader(t *testing.T) (*Upgrader, *fake.Clientset) {
	t.Helper()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: Deployment, Namespace: "ladder"},
		Spec: appsv1.DeploymentSpec{
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: Container, Image: "registry.local:5000/ladder-agent:1"},
			}}},
		},
	}
	cs := fake.NewClientset(d)
	u := New(&kube.Clients{Kube: cs}, "ladder", "1", 8081, slog.Default())
	u.Busy = func() string { return "" }
	return u, cs
}

func image(t *testing.T, cs *fake.Clientset) *appsv1.Deployment {
	t.Helper()
	d, err := cs.AppsV1().Deployments("ladder").Get(context.Background(), Deployment, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStart(t *testing.T) {
	ctx := context.Background()
	u, cs := newTestUpgrader(t)
	var got []protocol.AgentUpgrade
	u.OnStatus = func(s protocol.AgentUpgrade) { got = append(got, s) }

	for _, tc := range []struct{ version, code string }{
		{"1", protocol.CodeFailedPrecondition},
		{"evil/image:1", protocol.CodeBadRequest},
		{"", protocol.CodeBadRequest},
	} {
		_, err := u.Start(ctx, ctx, tc.version)
		var perr *protocol.Error
		if !errors.As(err, &perr) || perr.Code != tc.code {
			t.Errorf("Start(%q) = %v, want %s", tc.version, err, tc.code)
		}
	}
	u.Busy = func() string { return "setup is running" }
	if _, err := u.Start(ctx, ctx, "2"); err == nil {
		t.Error("Start while busy succeeded")
	}
	u.Busy = func() string { return "" }

	jobCtx, cancel := context.WithCancel(ctx)
	cancel() // the watch ends at once, as when Kubernetes stops the old agent
	st, err := u.Start(ctx, jobCtx, "2")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != protocol.UpgradeRunning || st.Image != "registry.local:5000/ladder-agent:2" || st.From != "1" || st.To != "2" {
		t.Errorf("status = %+v", st)
	}
	d := image(t, cs)
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != st.Image || c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Errorf("container = %+v", c)
	}
	if d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || d.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 0 {
		t.Errorf("strategy = %+v", d.Spec.Strategy)
	}
	if len(got) != 1 || got[0] != st {
		t.Errorf("OnStatus got %+v", got)
	}
}

func TestFailedNewPodRollsBack(t *testing.T) {
	ctx := context.Background()
	u, cs := newTestUpgrader(t)
	if err := u.patch(ctx, "registry.local:5000/ladder-agent:2"); err != nil {
		t.Fatal(err)
	}
	_, _ = cs.CoreV1().Pods("ladder").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ladder-agent-new", Namespace: "ladder", Labels: map[string]string{"app.kubernetes.io/name": Deployment}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: Container, Image: "registry.local:5000/ladder-agent:2"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  Container,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}},
		}}},
	}, metav1.CreateOptions{})

	st := protocol.AgentUpgrade{State: protocol.UpgradeRunning, From: "1", To: "2", Image: "registry.local:5000/ladder-agent:2"}
	problem := u.newPodProblem(ctx, st.Image)
	if problem != "pod ladder-agent-new: ImagePullBackOff: not found" {
		t.Fatalf("problem = %q", problem)
	}
	u.fail(st, "registry.local:5000/ladder-agent:1", problem)
	if img := image(t, cs).Spec.Template.Spec.Containers[0].Image; img != "registry.local:5000/ladder-agent:1" {
		t.Errorf("image after rollback = %s", img)
	}
	if s := u.Status(); s.State != protocol.UpgradeFailed || s.Error != problem {
		t.Errorf("status = %+v", s)
	}
}

// The agent runs a new version only by what Pin gives, such as a digest it
// checked the signature of, and not at all where updates are turned off.
func TestStartPins(t *testing.T) {
	ctx := context.Background()
	u, cs := newTestUpgrader(t)
	u.OnStatus = func(protocol.AgentUpgrade) {}
	var asked string
	var noRegistry bool
	u.Pin = func(_ context.Context, image string, local bool) (string, error) {
		asked, noRegistry = image, local
		if strings.HasSuffix(image, ":unsigned") {
			return "", errors.New("the image has no signature")
		}
		return "registry.local:5000/ladder-agent@sha256:" + strings.Repeat("a", 64), nil
	}
	var perr *protocol.Error
	if _, err := u.Start(ctx, ctx, "unsigned"); !errors.As(err, &perr) || perr.Code != protocol.CodeFailedPrecondition {
		t.Fatalf("unsigned: %v", err)
	}
	if got := image(t, cs).Spec.Template.Spec.Containers[0].Image; got != "registry.local:5000/ladder-agent:1" {
		t.Fatalf("unsigned image ran: %s", got)
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := u.Start(ctx, jobCtx, "2")
	if err != nil {
		t.Fatal(err)
	}
	if asked != "registry.local:5000/ladder-agent:2" || noRegistry {
		t.Errorf("Pin asked for %q, noRegistry %v", asked, noRegistry)
	}
	want := "registry.local:5000/ladder-agent@sha256:" + strings.Repeat("a", 64)
	if got := image(t, cs).Spec.Template.Spec.Containers[0].Image; got != want || st.Image != want || st.To != "2" {
		t.Errorf("runs %s, status %+v", got, st)
	}

	off, _ := newTestUpgrader(t)
	off.Allowed = false
	if _, err := off.Start(ctx, ctx, "2"); !errors.As(err, &perr) || perr.Code != protocol.CodeFailedPrecondition {
		t.Fatalf("updates off: %v", err)
	}
}

func TestHasRegistry(t *testing.T) {
	for repo, want := range map[string]bool{
		"ghcr.io/k8-ladder/ladder-agent": true, "registry.local:5000/ladder-agent": true,
		"localhost/ladder-agent": true, "ladder-agent": false, "k8-ladder/ladder-agent": false,
	} {
		if got := hasRegistry(repo); got != want {
			t.Errorf("%s: %v", repo, got)
		}
	}
}
