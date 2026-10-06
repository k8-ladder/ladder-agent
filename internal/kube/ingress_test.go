package kube

import (
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

func TestIngressAddresses(t *testing.T) {
	ing := &networkingv1.Ingress{}
	if got := IngressAddresses(ing); got != nil {
		t.Errorf("before the controller wrote the status: %v", got)
	}
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{
		{IP: "172.23.0.2"}, {IP: "172.23.0.3"}, {IP: "172.23.0.2"},
		{Hostname: "a1b2.elb.amazonaws.com", IP: "52.1.2.3"}, {},
	}
	want := []string{"172.23.0.2", "172.23.0.3", "a1b2.elb.amazonaws.com"}
	if got := IngressAddresses(ing); !slices.Equal(got, want) {
		t.Errorf("addresses = %v, want %v", got, want)
	}
}

func TestIngressHosts(t *testing.T) {
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{
		{Host: "shop.example.com"}, {}, {Host: "www.shop.example.com"}, {Host: "shop.example.com"},
	}}}
	want := []string{"shop.example.com", "www.shop.example.com"}
	if got := IngressHosts(ing); !slices.Equal(got, want) {
		t.Errorf("hosts = %v, want %v", got, want)
	}
	if got := IngressHosts(&networkingv1.Ingress{}); got == nil || len(got) != 0 {
		t.Errorf("hosts of an Ingress without rules = %#v, want empty", got)
	}
}
