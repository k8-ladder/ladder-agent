package kube

import (
	"slices"

	networkingv1 "k8s.io/api/networking/v1"
)

// IngressAddresses are the IPs and host names the ingress controller wrote
// into the Ingress's status, in its order and without repeats; none until it
// has. A cloud load balancer reports a host name (AWS) or an IP, k3s's
// servicelb the IP of each node.
func IngressAddresses(ing *networkingv1.Ingress) []string {
	var out []string
	for _, lb := range ing.Status.LoadBalancer.Ingress {
		a := lb.Hostname
		if a == "" {
			a = lb.IP
		}
		if a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// IngressHosts are the hosts of the Ingress's rules, without repeats. A rule
// without a host, which takes every host, adds none.
func IngressHosts(ing *networkingv1.Ingress) []string {
	out := []string{}
	for _, r := range ing.Spec.Rules {
		if r.Host != "" && !slices.Contains(out, r.Host) {
			out = append(out, r.Host)
		}
	}
	return out
}

// IngressTLSHosts are the hosts of the Ingress's TLS entries whose Secret
// holds a certificate, without repeats: the hosts it serves over HTTPS. An
// entry whose Secret is not there yet, such as one cert-manager has still to
// fill, adds none.
func IngressTLSHosts(ing *networkingv1.Ingress, hasCert func(secret string) bool) []string {
	out := []string{}
	for _, t := range ing.Spec.TLS {
		if t.SecretName == "" || !hasCert(t.SecretName) {
			continue
		}
		for _, h := range t.Hosts {
			if h != "" && !slices.Contains(out, h) {
				out = append(out, h)
			}
		}
	}
	return out
}
