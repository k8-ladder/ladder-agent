package argo

import (
	"reflect"
	"testing"

	"github.com/ladder/ladder-agent/internal/protocol"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAppIngresses(t *testing.T) {
	f := newFakeObjects()
	ing := func(name, host string, ips ...string) {
		i := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "dev", Name: name},
			Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: host}}}}
		for _, ip := range ips {
			i.Status.LoadBalancer.Ingress = append(i.Status.LoadBalancer.Ingress, networkingv1.IngressLoadBalancerIngress{IP: ip})
		}
		f.ings["dev/"+name] = i
	}
	ing("web", "shop.example.com", "203.0.113.10")
	ing("admin", "admin.shop.example.com")
	ing("other", "other.example.com", "203.0.113.10")
	top := []objKey{
		{"dev", "Deployment", "web"}, {"dev", "Ingress", "web"}, {"dev", "Ingress", "admin"},
		{"dev", "Ingress", "gone"},
	}

	want := []protocol.Ingress{
		{Name: "admin", Hosts: []string{"admin.shop.example.com"}},
		{Name: "web", Hosts: []string{"shop.example.com"}, Addresses: []string{"203.0.113.10"}},
	}
	if got := appIngresses(f, top); !reflect.DeepEqual(got, want) {
		t.Errorf("ingresses = %+v, want %+v", got, want)
	}
	if got := appIngresses(f, []objKey{{"dev", "Deployment", "web"}}); got != nil {
		t.Errorf("ingresses of an app without one = %+v, want nil", got)
	}
}
