package argo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/k8-ladder/ladder-agent/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func argoObject(kind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("argoproj.io/v1alpha1")
	u.SetKind(kind)
	u.SetNamespace("argocd")
	u.SetName(name)
	return u
}

var lists = map[schema.GroupVersionResource]string{
	kube.ApplicationGVR:    "ApplicationList",
	kube.ApplicationSetGVR: "ApplicationSetList",
}

// Applications learn of the push from Argo CD's webhook, which the agent
// posts as a git server would; ApplicationSets from their annotation.
func TestRefresh(t *testing.T) {
	ctx := context.Background()
	var got struct {
		event string
		body  map[string]any
	}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.event = r.Header.Get("X-Gogs-Event")
		json.NewDecoder(r.Body).Decode(&got.body)
	}))
	defer hook.Close()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists,
		argoObject("Application", "shop-api"), argoObject("ApplicationSet", "apps"))
	r := &Refresher{K: &kube.Clients{Dynamic: dyn}, Client: hook.Client(), Webhook: hook.URL}
	if err := r.Refresh(ctx, "argocd", "http://ladder.example.com/git/acme/prod.git"); err != nil {
		t.Fatal(err)
	}
	repo, _ := got.body["repository"].(map[string]any)
	if got.event != "push" || got.body["ref"] != "refs/heads/main" || repo["html_url"] != "http://ladder.example.com/git/acme/prod" {
		t.Errorf("webhook got %q %v", got.event, got.body)
	}
	app, _ := dyn.Resource(kube.ApplicationGVR).Namespace("argocd").Get(ctx, "shop-api", metav1.GetOptions{})
	set, _ := dyn.Resource(kube.ApplicationSetGVR).Namespace("argocd").Get(ctx, "apps", metav1.GetOptions{})
	if len(app.GetAnnotations()) != 0 || set.GetAnnotations()["argocd.argoproj.io/application-set-refresh"] != "true" {
		t.Errorf("annotations: %v, %v", app.GetAnnotations(), set.GetAnnotations())
	}

	// Without the webhook, the Applications are annotated, as an agent with
	// setup access may.
	r.Webhook = hook.URL + "/gone"
	hook.Config.Handler = http.NotFoundHandler()
	if err := r.Refresh(ctx, "argocd", "http://ladder.example.com/git/acme/prod.git"); err != nil {
		t.Fatal(err)
	}
	app, _ = dyn.Resource(kube.ApplicationGVR).Namespace("argocd").Get(ctx, "shop-api", metav1.GetOptions{})
	if app.GetAnnotations()["argocd.argoproj.io/refresh"] != "normal" {
		t.Errorf("fallback annotations: %v", app.GetAnnotations())
	}

	// Before setup installs Argo CD, the cluster has no such resources.
	none := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)
	none.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
	})
	r = &Refresher{K: &kube.Clients{Dynamic: none}, Client: hook.Client(), Webhook: hook.URL}
	if err := r.Refresh(ctx, "argocd", ""); err != nil {
		t.Errorf("without Argo CD: %v", err)
	}
}

func TestWebURL(t *testing.T) {
	for in, want := range map[string]string{
		"ssh://git@github.com/acme/prod.git":        "https://github.com/acme/prod",
		"git@github.com:acme/prod.git":              "https://github.com/acme/prod",
		"https://ladder.example.com/git/acme/p.git": "https://ladder.example.com/git/acme/p",
		"http://host.k3d.internal:8081/git/a/b.git": "http://host.k3d.internal:8081/git/a/b",
	} {
		if got := WebURL(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
