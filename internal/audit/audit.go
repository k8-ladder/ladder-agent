// Package audit records in the cluster what the cloud asked the agent to do
// that changes the cluster or reads more than the apps' status (spec §12):
// as an Event about the agent's Deployment, and as a line in ConfigMap
// ladder-audit, which keeps the last lines. The cloud has no method that
// changes either, so a compromised cloud leaves its traces where the
// people who run the cluster look: kubectl -n ladder get events, and
// kubectl -n ladder get configmap ladder-audit -o jsonpath='{.data.log}'.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ladder/ladder-agent/internal/link"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ConfigMap holds the last lines.
	ConfigMap = "ladder-audit"
	// Keep is how many lines the ConfigMap keeps.
	Keep = 500

	deployment = "ladder-agent"
)

// Log records requests.
type Log struct {
	Kube      kubernetes.Interface
	Namespace string
	Pod       string
	Log       *slog.Logger
	Now       func() time.Time

	mu  sync.Mutex
	seq atomic.Uint32
}

// Describe says what a request asks for, without secrets: the method's
// arguments that matter, never values.
type Describe func(payload json.RawMessage) string

// Wrap records each call of h, with what describe says of it and whether it
// was served.
func (l *Log) Wrap(method string, describe Describe, h link.Handler) link.Handler {
	return func(ctx context.Context, payload json.RawMessage) (any, error) {
		res, err := h(ctx, payload)
		detail := ""
		if describe != nil {
			detail = describe(payload)
		}
		l.Record(method, detail, err)
		return res, err
	}
}

// Record writes one line, best effort: a failure to record is logged and
// does not fail the request.
func (l *Log) Record(method, detail string, failed error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	t := now().UTC()
	outcome := "served"
	if failed != nil {
		outcome = "refused: " + oneLine(failed.Error())
	}
	line := fmt.Sprintf("%s %s %s: %s", t.Format(time.RFC3339), method, oneLine(detail), outcome)
	l.Log.Info("audit", "method", method, "detail", detail, "outcome", outcome)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.event(ctx, t, method, detail, outcome); err != nil {
		l.Log.Warn("audit event", "err", err)
	}
	if err := l.appendLine(ctx, line); err != nil {
		l.Log.Warn("audit log", "err", err)
	}
}

func (l *Log) event(ctx context.Context, t time.Time, method, detail, outcome string) error {
	reason := "CloudRequest"
	typ := corev1.EventTypeNormal
	if strings.HasPrefix(outcome, "refused") {
		reason, typ = "CloudRequestRefused", corev1.EventTypeWarning
	}
	msg := method
	if detail != "" {
		msg += " " + detail
	}
	msg += ": " + outcome
	_, err := l.Kube.CoreV1().Events(l.Namespace).Create(ctx, &corev1.Event{
		// Named as Kubernetes names events: the object and a time.
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s.%x%02x", deployment, t.UnixNano(), l.seq.Add(1)%256), Namespace: l.Namespace},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "apps/v1", Kind: "Deployment", Namespace: l.Namespace, Name: deployment,
		},
		Reason: reason, Message: truncate(msg, 1024), Type: typ,
		Source:              corev1.EventSource{Component: deployment},
		ReportingController: "ladder.dev/agent", ReportingInstance: l.Pod,
		FirstTimestamp: metav1.NewTime(t), LastTimestamp: metav1.NewTime(t), Count: 1,
	}, metav1.CreateOptions{})
	return err
}

// appendLine adds a line to the ConfigMap and drops the oldest past Keep.
func (l *Log) appendLine(ctx context.Context, line string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cms := l.Kube.CoreV1().ConfigMaps(l.Namespace)
	for range 3 {
		cm, err := cms.Get(ctx, ConfigMap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cms.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: ConfigMap, Namespace: l.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "ladder"}},
				Data:       map[string]string{"log": line + "\n"},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["log"] = keepLast(cm.Data["log"]+line+"\n", Keep)
		_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("ConfigMap %s keeps changing", ConfigMap)
}

func keepLast(log string, n int) string {
	lines := strings.SplitAfter(log, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Field describes a payload by some of its JSON fields, as name=value.
func Field(names ...string) Describe {
	return func(payload json.RawMessage) string {
		var m map[string]any
		if json.Unmarshal(payload, &m) != nil {
			return ""
		}
		var parts []string
		for _, n := range names {
			if v, ok := m[n]; ok && v != "" && v != nil {
				parts = append(parts, fmt.Sprintf("%s=%v", n, v))
			}
		}
		return strings.Join(parts, " ")
	}
}
