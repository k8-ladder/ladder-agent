// Package upgrade replaces the agent with another version of its image. It
// points the agent's Deployment at the new tag and watches the rollout. The
// Deployment rolls with maxUnavailable 0 and the new pod is ready only once
// it has connected to the cloud, so this agent keeps running until the new
// one works. If the new version does not start, the upgrader points the
// Deployment back at the old image.
package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// Deployment is the agent's Deployment, and Container its container, in
	// the agent's namespace.
	Deployment = "ladder-agent"
	Container  = "agent"

	pollInterval = 3 * time.Second
	// readyTimeout covers pulling the image and connecting to the cloud.
	readyTimeout = 5 * time.Minute
)

// tagRe is a Docker image tag.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// Waiting reasons of a container that will not start without a change.
var stuckReasons = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true, "ErrImageNeverPull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true,
}

type Upgrader struct {
	k         *kube.Clients
	namespace string
	version   string
	// port serves /readyz, the new pod's readiness probe.
	port int
	log  *slog.Logger

	// OnStatus receives every change of the upgrade.
	// Allowed is false where the people who run the cluster turned off
	// updates from the cloud.
	Allowed bool
	// Pin checks the image and returns the reference to run it by, such as
	// repository@digest; nil runs it by tag. noRegistry says the agent's
	// own image is in no registry, as install-local.sh imports it.
	Pin func(ctx context.Context, image string, noRegistry bool) (string, error)

	OnStatus func(protocol.AgentUpgrade)
	// Busy returns why the agent cannot be replaced now, or "".
	Busy func() string

	mu     sync.Mutex
	status *protocol.AgentUpgrade
}

func New(k *kube.Clients, namespace, version string, port int, log *slog.Logger) *Upgrader {
	return &Upgrader{k: k, namespace: namespace, version: version, port: port, log: log, Allowed: true}
}

// Status is the last upgrade, or nil.
func (u *Upgrader) Status() *protocol.AgentUpgrade {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.status == nil {
		return nil
	}
	s := *u.status
	return &s
}

// Start points the Deployment at version and watches the rollout in the
// background, until jobCtx ends: when Kubernetes stops this agent because the
// new one is ready, the upgrade is done. Only a tag of the image this agent
// runs from is accepted, so the cloud cannot make the agent run another image.
func (u *Upgrader) Start(ctx, jobCtx context.Context, version string) (protocol.AgentUpgrade, error) {
	if !u.Allowed {
		return protocol.AgentUpgrade{}, protocol.Errorf(protocol.CodeFailedPrecondition,
			"updates from Ladder are turned off in this cluster (LADDER_REMOTE_UPGRADE=off): kubectl -n %s set image deploy/%s %s=<image>", u.namespace, Deployment, Container)
	}
	if !tagRe.MatchString(version) {
		return protocol.AgentUpgrade{}, protocol.Errorf(protocol.CodeBadRequest, "%q is not an image tag", version)
	}
	if version == u.version {
		return protocol.AgentUpgrade{}, protocol.Errorf(protocol.CodeFailedPrecondition, "the agent already runs version %s", version)
	}
	if reason := u.Busy(); reason != "" {
		return protocol.AgentUpgrade{}, protocol.Errorf(protocol.CodeConflict, "cannot upgrade now: %s", reason)
	}
	u.mu.Lock()
	running := u.status != nil && u.status.State == protocol.UpgradeRunning
	u.mu.Unlock()
	if running {
		return protocol.AgentUpgrade{}, protocol.Errorf(protocol.CodeConflict, "an upgrade is already running")
	}

	d, err := u.k.Kube.AppsV1().Deployments(u.namespace).Get(ctx, Deployment, metav1.GetOptions{})
	if err != nil {
		return protocol.AgentUpgrade{}, fmt.Errorf("read deployment %s: %w", Deployment, err)
	}
	old := ""
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == Container {
			old = c.Image
		}
	}
	if old == "" {
		return protocol.AgentUpgrade{}, fmt.Errorf("deployment %s has no container %s", Deployment, Container)
	}
	image := repository(old) + ":" + version
	if u.Pin != nil {
		pinned, err := u.Pin(ctx, image, !hasRegistry(repository(old)))
		if err != nil {
			return protocol.AgentUpgrade{}, protocol.Errorf(protocol.CodeFailedPrecondition, "the agent does not run %s: %v", image, err)
		}
		image = pinned
	}
	if err := u.patch(ctx, image); err != nil {
		return protocol.AgentUpgrade{}, err
	}
	u.log.Info("upgrade started", "from", u.version, "to", version, "image", image)
	st := protocol.AgentUpgrade{State: protocol.UpgradeRunning, From: u.version, To: version, Image: image}
	u.set(st)
	go u.watch(jobCtx, st, old)
	return st, nil
}

// patch sets the image, and what an older install lacks to keep this agent
// until the new one is ready: the rollout settings, the readiness probe, and
// POD_NAME for WaitAlone.
func (u *Upgrader) patch(ctx context.Context, image string) error {
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{
		"strategy": map[string]any{
			"type":          "RollingUpdate",
			"rollingUpdate": map[string]any{"maxUnavailable": 0, "maxSurge": 1},
		},
		"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
			"name":  Container,
			"image": image,
			"env": []any{map[string]any{
				"name":      "POD_NAME",
				"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}},
			}},
			"readinessProbe": map[string]any{
				"httpGet":       map[string]any{"path": "/readyz", "port": u.port},
				"periodSeconds": 2,
			},
		}}}},
	}})
	_, err := u.k.Kube.AppsV1().Deployments(u.namespace).Patch(ctx, Deployment, types.StrategicMergePatchType, patch,
		metav1.PatchOptions{FieldManager: kube.FieldManager})
	if err != nil {
		return fmt.Errorf("update deployment %s: %w", Deployment, err)
	}
	return nil
}

// watch fails the upgrade when the new pods cannot start or do not get ready
// in time, and then points the Deployment back at old.
func (u *Upgrader) watch(ctx context.Context, st protocol.AgentUpgrade, old string) {
	deadline := time.After(readyTimeout)
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return // Kubernetes stops this agent: the new one is ready
		case <-deadline:
			u.fail(st, old, fmt.Sprintf("the new agent was not ready after %s; it is ready once it has connected to the cloud", readyTimeout))
			return
		case <-t.C:
			if problem := u.newPodProblem(ctx, st.Image); problem != "" {
				u.fail(st, old, problem)
				return
			}
		}
	}
}

// newPodProblem says why a pod of image cannot run, or "".
func (u *Upgrader) newPodProblem(ctx context.Context, image string) string {
	pods, err := u.k.Kube.CoreV1().Pods(u.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + Deployment})
	if err != nil {
		u.log.Warn("list agent pods", "err", err)
		return ""
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.ContainerStatuses {
			if c.Name != Container || !specImage(&p, image) {
				continue
			}
			if w := c.State.Waiting; w != nil && stuckReasons[w.Reason] {
				return fmt.Sprintf("pod %s: %s: %s", p.Name, w.Reason, w.Message)
			}
			if t := c.LastTerminationState.Terminated; c.RestartCount > 0 && t != nil {
				return fmt.Sprintf("pod %s: the new agent exited: %s, code %d", p.Name, or(t.Reason, "Error"), t.ExitCode)
			}
		}
	}
	return ""
}

func (u *Upgrader) fail(st protocol.AgentUpgrade, old, problem string) {
	u.log.Error("upgrade failed, rolling back", "to", st.To, "problem", problem)
	st.State, st.Error = protocol.UpgradeFailed, problem
	// The request's context is gone; rolling back must not depend on it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := u.patch(ctx, old); err != nil {
		st.Error += fmt.Sprintf(". Rolling back failed too: %v; run kubectl -n %s rollout undo deploy/%s", err, u.namespace, Deployment)
	}
	u.set(st)
}

func (u *Upgrader) set(st protocol.AgentUpgrade) {
	u.mu.Lock()
	u.status = &st
	u.mu.Unlock()
	if u.OnStatus != nil {
		u.OnStatus(st)
	}
}

// specImage reports whether the pod's agent container runs image. The spec,
// not the status: the status has the image as the runtime resolved it.
func specImage(p *corev1.Pod, image string) bool {
	for _, c := range p.Spec.Containers {
		if c.Name == Container {
			return c.Image == image
		}
	}
	return false
}

// repository strips the tag and digest from an image reference. A colon
// before the last slash belongs to a registry port.
func repository(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	return image
}

// hasRegistry reports whether repository names a registry, as
// ghcr.io/k8-ladder/ladder-agent does and ladder-agent does not.
func hasRegistry(repository string) bool {
	host, _, ok := strings.Cut(repository, "/")
	return ok && (strings.ContainsAny(host, ".:") || host == "localhost")
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// WaitAlone returns once pod is the only pod of the agent's Deployment, so
// that work only one agent may do, such as setup, can start. Without pod, as
// when the agent runs outside the cluster, it returns at once.
func WaitAlone(ctx context.Context, k *kube.Clients, namespace, pod string) error {
	if pod == "" {
		return nil
	}
	return kube.Poll(ctx, pollInterval, func() (bool, error) {
		pods, err := k.Kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=" + Deployment})
		if err != nil {
			return false, nil
		}
		for _, p := range pods.Items {
			if p.Name != pod {
				return false, nil
			}
		}
		return true, nil
	})
}
