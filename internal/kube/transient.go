package kube

import (
	"context"
	"errors"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

// Transient says whether err is the cluster's API failing to answer, not
// refusing the request: a dropped connection, a timeout, an API server that is
// busy, restarting or failing. Calling again may succeed. A new managed
// cluster, such as one on DigitalOcean, drops connections now and then in its
// first minutes. An ended context is not transient.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if utilnet.IsProbableEOF(err) || utilnet.IsConnectionReset(err) || utilnet.IsConnectionRefused(err) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) || apierrors.IsUnexpectedServerError(err)
}
