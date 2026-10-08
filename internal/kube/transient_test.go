package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestTransient(t *testing.T) {
	sa := schema.GroupResource{Resource: "serviceaccounts"}
	patch := func(err error) error {
		return &url.Error{Op: "Patch", URL: "https://10.110.32.1:443/api/v1/namespaces/argocd/serviceaccounts/argocd-redis-secret-init", Err: err}
	}
	// helmHook wraps err as Helm 4 does for a hook it could not create.
	helmHook := func(err error) error {
		err = fmt.Errorf("server-side apply failed for object argocd/argocd-redis-secret-init /v1, Kind=ServiceAccount: %w", err)
		err = fmt.Errorf("warning: Hook pre-install argo-cd/templates/redis-secret-init/serviceaccount.yaml failed: %w", errors.Join(err))
		return fmt.Errorf("helm install argocd: %w", fmt.Errorf("failed pre-install: %w", err))
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"connection dropped in a Helm hook", helmHook(patch(io.ErrUnexpectedEOF)), true},
		{"connection reset", patch(syscall.ECONNRESET), true},
		{"connection refused", patch(syscall.ECONNREFUSED), true},
		{"API server busy", apierrors.NewTooManyRequests("busy", 1), true},
		{"API server unavailable", apierrors.NewServiceUnavailable("starting"), true},
		{"API server timeout", apierrors.NewTimeoutError("slow", 1), true},
		{"API server failing", apierrors.NewInternalError(errors.New("etcd")), true},
		{"not found", apierrors.NewNotFound(sa, "argocd-server"), false},
		{"forbidden", apierrors.NewForbidden(sa, "argocd-server", errors.New("no")), false},
		{"invalid", apierrors.NewBadRequest("invalid values"), false},
		{"step timed out", patch(context.DeadlineExceeded), false},
		{"agent stopping", fmt.Errorf("wait: %w", context.Canceled), false},
		{"other", errors.New("release argocd is failed"), false},
		{"no error", nil, false},
	}
	for _, c := range cases {
		if got := Transient(c.err); got != c.want {
			t.Errorf("%s: Transient(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}
