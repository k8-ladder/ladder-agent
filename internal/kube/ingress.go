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
