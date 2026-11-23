package appsecrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func request() protocol.AppSecretsCreate {
	return protocol.AppSecretsCreate{App: "shop-api", Namespace: "shop-prod", Name: "ladder-shop-api-env-0123456789abcdef0123456789abcdef", Values: map[string]string{"value-0": "sentinel-secret"}}
}

func TestCreateAndRetry(t *testing.T) {
	kc := fake.NewClientset()
	k := &kube.Clients{Kube: kc}
	ctx, req := context.Background(), request()
	for range 2 {
		if err := Create(ctx, k, req); err != nil {
			t.Fatal(err)
		}
	}
	s, err := kc.CoreV1().Secrets(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Immutable == nil || !*s.Immutable || string(s.Data["value-0"]) != "sentinel-secret" || s.Labels["ladder.dev/app-secrets"] != req.App || len(s.Annotations) != 0 {
		t.Fatalf("Secret metadata/data incorrect")
	}
	req.Values["value-0"] = "replacement-secret"
	if err := Create(ctx, k, req); err == nil || strings.Contains(err.Error(), "replacement-secret") {
		t.Fatalf("immutable replacement error: %v", err)
	}
	s, _ = kc.CoreV1().Secrets(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if string(s.Data["value-0"]) != "sentinel-secret" {
		t.Fatal("original version changed")
	}
}

func TestRejectBeforeWriting(t *testing.T) {
	for _, alter := range []func(*protocol.AppSecretsCreate){
		func(r *protocol.AppSecretsCreate) { r.Namespace = "kube-system" },
		func(r *protocol.AppSecretsCreate) { r.Namespace = "argocd" },
		func(r *protocol.AppSecretsCreate) { r.Namespace = "ladder" },
		func(r *protocol.AppSecretsCreate) { r.Name = "somebody-elses-secret" },
		func(r *protocol.AppSecretsCreate) { r.App = "../../ladder" },
		func(r *protocol.AppSecretsCreate) { r.Values["bad/key"] = "secret" },
		func(r *protocol.AppSecretsCreate) { r.Values["value-0"] = strings.Repeat("x", 512<<10) },
	} {
		kc := fake.NewClientset()
		r := request()
		alter(&r)
		if err := Create(context.Background(), &kube.Clients{Kube: kc}, r); err == nil {
			t.Fatal("invalid request accepted")
		}
		if len(kc.Actions()) != 0 {
			t.Fatal("invalid request touched the cluster")
		}
	}
}

func TestAdmissionErrorDoesNotLeakValues(t *testing.T) {
	kc := fake.NewClientset()
	kc.PrependReactor("create", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("admission error: sentinel-secret")
	})
	err := Create(context.Background(), &kube.Clients{Kube: kc}, request())
	if err == nil || strings.Contains(err.Error(), "sentinel-secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}

// The namespace the agent makes for an app is labelled as one of apps, and a
// namespace Ladder did not make gets no Secret.
func TestNamespaceOfApps(t *testing.T) {
	kc := fake.NewClientset()
	k := &kube.Clients{Kube: kc}
	ctx, req := context.Background(), request()
	if err := Create(ctx, k, req); err != nil {
		t.Fatal(err)
	}
	ns, err := kc.CoreV1().Namespaces().Get(ctx, req.Namespace, metav1.GetOptions{})
	if err != nil || ns.Labels["ladder.dev/apps"] != "true" {
		t.Fatalf("namespace = %v, %v", ns, err)
	}
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "billing"}}
	kc = fake.NewClientset(other)
	k = &kube.Clients{Kube: kc}
	req.Namespace = "billing"
	var perr *protocol.Error
	if err := Create(ctx, k, req); !errors.As(err, &perr) || perr.Code != protocol.CodeFailedPrecondition {
		t.Fatalf("unlabelled namespace: %v", err)
	}
	if list, _ := kc.CoreV1().Secrets("billing").List(ctx, metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatal("Secret written into a namespace Ladder did not make")
	}
}
