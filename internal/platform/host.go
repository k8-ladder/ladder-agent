package platform

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/ladder/ladder-agent/internal/kube"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TLSSecret holds the certificate of Argo CD's host.
const TLSSecret = "argocd-server-tls"

var (
	ingressGVR = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	cgnat      = netip.MustParsePrefix("100.64.0.0/10")
)

// SSLIPHost is Argo CD's host at the first public IPv4 address, which
// sslip.io resolves to it: argocd.203-0-113-7.sslip.io. Empty without one.
func SSLIPHost(addresses []string) string {
	for _, a := range addresses {
		ip, err := netip.ParseAddr(a)
		if err != nil || !ip.Unmap().Is4() {
			continue
		}
		ip = ip.Unmap()
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || cgnat.Contains(ip) {
			continue
		}
		return "argocd." + strings.ReplaceAll(ip.String(), ".", "-") + ".sslip.io"
	}
	return ""
}

// WatchHost gives Argo CD's Ingress, where Ladder added Traefik, the host
// argocd.<address>.sslip.io and a tls entry once Traefik's load balancer has
// a public address (spec §5.6). cert-manager then gets the certificate. It
// does it once: an Ingress with a host or a tls entry is left as it is. It
// returns when done or when ctx ends.
func WatchHost(ctx context.Context, k *kube.Clients, namespace string, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		done, err := SetHost(ctx, k, namespace, log)
		if err != nil && ctx.Err() == nil {
			log.Warn("argo cd host", "err", err)
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SetHost gives the Ingress its host if it has an address and no host yet,
// and reports whether nothing is left to do.
func SetHost(ctx context.Context, k *kube.Clients, namespace string, log *slog.Logger) (bool, error) {
	ings := k.Kube.NetworkingV1().Ingresses(namespace)
	ing, err := ings.Get(ctx, Ingress, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(ing.Spec.TLS) > 0 || len(ing.Spec.Rules) != 1 {
		return true, nil
	}
	if ing.Spec.Rules[0].Host != "" {
		return true, nil
	}
	host := SSLIPHost(kube.IngressAddresses(ing))
	if host == "" {
		return false, nil
	}
	ing.Spec.Rules[0].Host = host
	ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: TLSSecret}}
	if _, err := ings.Update(ctx, ing, metav1.UpdateOptions{FieldManager: kube.FieldManager}); err != nil {
		return false, err
	}
	log.Info("argo cd host set", "host", host)
	return true, nil
}
