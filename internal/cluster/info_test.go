package cluster

import (
	"slices"
	"testing"

	"github.com/k8-ladder/ladder-agent/internal/kube"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Argo CD's Ingress reports its hosts, and serves HTTPS on those whose
// Secret has a certificate: cert-manager writes it after Let's Encrypt
// issued it.
func TestArgoCDIngress(t *testing.T) {
	host := "argocd.203-0-113-7.sslip.io"
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "argocd-server", Namespace: "argocd"},
		Spec: networkingv1.IngressSpec{
			TLS:   []networkingv1.IngressTLS{{Hosts: []string{host}, SecretName: "argocd-server-tls"}},
			Rules: []networkingv1.IngressRule{{Host: host}},
		},
		Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
			Ingress: []networkingv1.IngressLoadBalancerIngress{{IP: "203.0.113.7"}},
		}},
	}
	cs := fake.NewClientset(ing)
	i := NewInspector(&kube.Clients{Kube: cs})

	addrs, hosts, tls := i.ingress(t.Context(), "argocd")
	if !slices.Equal(addrs, []string{"203.0.113.7"}) || !slices.Equal(hosts, []string{host}) || len(tls) != 0 {
		t.Errorf("before the certificate: %v %v %v", addrs, hosts, tls)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "argocd-server-tls", Namespace: "argocd"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	if _, err := cs.CoreV1().Secrets("argocd").Create(t.Context(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, tls := i.ingress(t.Context(), "argocd"); !slices.Equal(tls, []string{host}) {
		t.Errorf("TLS hosts with the certificate = %v", tls)
	}

	if addrs, hosts, tls := i.ingress(t.Context(), "elsewhere"); addrs != nil || hosts != nil || tls != nil {
		t.Errorf("without the Ingress: %v %v %v", addrs, hosts, tls)
	}

	// An Ingress that takes every host reports no hosts, but says so: the
	// cloud gives Argo CD its host only then.
	hostless := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "argocd-server", Namespace: "plain"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{}}}}
	if _, err := cs.NetworkingV1().Ingresses("plain").Create(t.Context(), hostless, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, hosts, _ := i.ingress(t.Context(), "plain"); hosts == nil || len(hosts) != 0 {
		t.Errorf("hosts of a hostless Ingress = %#v, want empty", hosts)
	}
}
