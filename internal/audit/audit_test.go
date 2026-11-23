package audit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Each call leaves an Event and a line, with what was asked and how it
// went, and never a value it carries.
func TestWrap(t *testing.T) {
	kc := fake.NewClientset()
	l := &Log{Kube: kc, Namespace: "ladder", Pod: "ladder-agent-1", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) }}
	ok := l.Wrap("agent.upgrade", Field("version"), func(context.Context, json.RawMessage) (any, error) { return "x", nil })
	bad := l.Wrap("kube.read", Field("op", "kind", "namespace"), func(context.Context, json.RawMessage) (any, error) {
		return nil, errors.New("forbidden\nby RBAC")
	})
	if res, err := ok(context.Background(), json.RawMessage(`{"version":"2","secret":"s3cret"}`)); res != "x" || err != nil {
		t.Fatalf("handler result changed: %v %v", res, err)
	}
	if _, err := bad(context.Background(), json.RawMessage(`{"op":"get","kind":"secrets","namespace":"argocd"}`)); err == nil {
		t.Fatal("handler error lost")
	}
	cm, err := kc.CoreV1().ConfigMaps("ladder").Get(context.Background(), ConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "2026-11-23T12:00:00Z agent.upgrade version=2: served\n2026-11-23T12:00:00Z kube.read op=get kind=secrets namespace=argocd: refused: forbidden by RBAC\n"
	if cm.Data["log"] != want {
		t.Errorf("log =\n%s\nwant\n%s", cm.Data["log"], want)
	}
	events, _ := kc.CoreV1().Events("ladder").List(context.Background(), metav1.ListOptions{})
	if len(events.Items) != 2 || events.Items[1].Type != "Warning" || events.Items[0].InvolvedObject.Name != "ladder-agent" {
		t.Errorf("events = %+v", events.Items)
	}
	for _, e := range events.Items {
		if strings.Contains(e.Message, "s3cret") {
			t.Error("an event carries a value of the request")
		}
	}
}

func TestKeepLast(t *testing.T) {
	var b strings.Builder
	for i := range Keep + 10 {
		b.WriteString(strings.Repeat("x", i%3) + "\n")
	}
	if got := strings.Count(keepLast(b.String(), Keep), "\n"); got != Keep {
		t.Errorf("kept %d lines", got)
	}
}
