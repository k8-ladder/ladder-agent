package argo

import (
	"slices"
	"strings"

	"github.com/k8-ladder/ladder-agent/internal/kube"
	"github.com/k8-ladder/ladder-agent/protocol"
)

// appIngresses are the Ingresses Argo CD manages for the app, by name, with
// their hosts and the addresses their ingress controller reported: where the
// app's hosts must point in DNS. Nil for an app without one.
func appIngresses(o objects, top []objKey) []protocol.Ingress {
	var out []protocol.Ingress
	for _, k := range top {
		if k.Kind != "Ingress" {
			continue
		}
		ing := o.ingress(k.Namespace, k.Name)
		if ing == nil {
			continue
		}
		out = append(out, protocol.Ingress{
			Name:      ing.Name,
			Hosts:     kube.IngressHosts(ing),
			Addresses: kube.IngressAddresses(ing),
		})
	}
	slices.SortFunc(out, func(a, b protocol.Ingress) int { return strings.Compare(a.Name, b.Name) })
	return out
}
