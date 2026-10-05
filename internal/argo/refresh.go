package argo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ladder/ladder-agent/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Refresh makes Argo CD read git now instead of at its next poll (about three
// minutes): every ApplicationSet is generated again, so a new app gets its
// Application, and every Application is compared with the new commit. Argo CD
// removes the annotations once it has acted on them. Called on a push to main.
func Refresh(ctx context.Context, k *kube.Clients, namespace string) error {
	var errs []error
	for _, r := range []struct {
		gvr               schema.GroupVersionResource
		annotation, value string
	}{
		{kube.ApplicationSetGVR, "argocd.argoproj.io/application-set-refresh", "true"},
		{kube.ApplicationGVR, "argocd.argoproj.io/refresh", "normal"},
	} {
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{r.annotation: r.value}}})
		ri := k.Dynamic.Resource(r.gvr).Namespace(namespace)
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, o := range list.Items {
			_, err := ri.Patch(ctx, o.GetName(), types.MergePatchType, patch, metav1.PatchOptions{FieldManager: kube.FieldManager})
			if err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
