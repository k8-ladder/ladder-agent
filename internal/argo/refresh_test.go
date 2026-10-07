package argo

import (
	"context"
	"testing"

	"github.com/ladder/ladder-agent/internal/kube"
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

func TestRefresh(t *testing.T) {
	ctx := context.Background()
	lists := map[schema.GroupVersionResource]string{
		kube.ApplicationGVR:    "ApplicationList",
		kube.ApplicationSetGVR: "ApplicationSetList",
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists,
		argoObject("Application", "shop-api"), argoObject("ApplicationSet", "apps"))
	k := &kube.Clients{Dynamic: dyn}
	if err := Refresh(ctx, k, "argocd"); err != nil {
		t.Fatal(err)
	}
	app, _ := dyn.Resource(kube.ApplicationGVR).Namespace("argocd").Get(ctx, "shop-api", metav1.GetOptions{})
	set, _ := dyn.Resource(kube.ApplicationSetGVR).Namespace("argocd").Get(ctx, "apps", metav1.GetOptions{})
	if app.GetAnnotations()["argocd.argoproj.io/refresh"] != "normal" ||
		set.GetAnnotations()["argocd.argoproj.io/application-set-refresh"] != "true" {
		t.Errorf("annotations: %v, %v", app.GetAnnotations(), set.GetAnnotations())
	}

	// Before setup installs Argo CD, the cluster has no such resources.
	none := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)
	none.PrependReactor("list", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
	})
	if err := Refresh(ctx, &kube.Clients{Dynamic: none}, "argocd"); err != nil {
		t.Errorf("without Argo CD: %v", err)
	}
}
