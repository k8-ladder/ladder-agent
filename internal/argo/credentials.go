package argo

import (
	"context"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InitialCredentials reads the admin password Argo CD generated on install
// (spec §5.6). The Secret is gone once someone changes the password and
// deletes it, as Argo CD recommends.
func InitialCredentials(ctx context.Context, k *kube.Clients, namespace string) (*protocol.ArgoCDCredentials, error) {
	s, err := k.Kube.CoreV1().Secrets(namespace).Get(ctx, "argocd-initial-admin-secret", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, protocol.Errorf(protocol.CodeFailedPrecondition,
			"Secret argocd-initial-admin-secret is not in namespace %s: Argo CD is not installed yet, or its admin password was changed", namespace)
	}
	if err != nil {
		return nil, err
	}
	return &protocol.ArgoCDCredentials{Username: "admin", Password: string(s.Data["password"])}, nil
}
