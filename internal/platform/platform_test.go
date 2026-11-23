package platform

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/ladder/ladder-agent/internal/kube"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func params() Params {
	return Params{
		ArgoCDNamespace: "argocd", ArgoCDChartRepo: "https://argoproj.github.io/argo-helm",
		ArgoCDChartVersion: "10.9.6", ArgoCDVersion: "3.5.3", AgentNamespace: "ladder",
		RepoURL: "https://ladder.example.com/git/acme/prod.git", Exposure: "ingress",
		Viewer: Viewer{PasswordHash: "$2a$10$hash", PasswordMtime: "2026-11-23T10:00:00Z"},
	}
}

func names(pl *Platform) []string {
	var out []string
	for _, o := range pl.Objects {
		out = append(out, o.GetKind()+"/"+o.GetName())
	}
	return out
}

func find(t *testing.T, pl *Platform, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range pl.Objects {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s %s in %v", kind, name, names(pl))
	return nil
}

func TestRender(t *testing.T) {
	pl, err := Render(params())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ServiceAccount/ladder-apps-deployer", "ClusterRoleBinding/ladder-apps-deployer", "ClusterRole/ladder-apps-deployer",
		"ClusterRoleBinding/ladder-apps-deployer-namespaces", "ServiceAccount/ladder-platform-deployer",
		"ClusterRoleBinding/ladder-platform-deployer", "AppProject/default", "AppProject/platform", "AppProject/apps",
		"ApplicationSet/apps", "ValidatingAdmissionPolicy/ladder-apps-baseline", "ValidatingAdmissionPolicyBinding/ladder-apps-baseline",
		"ValidatingAdmissionPolicy/ladder-apps-namespaces", "ValidatingAdmissionPolicyBinding/ladder-apps-namespaces",
		"Application/argocd",
	}
	if got := names(pl); !slices.Equal(got, want) {
		t.Errorf("objects =\n%v\nwant\n%v", got, want)
	}
	// Argo CD's values: impersonation on, the read-only account, no Ingress
	// in them (the agent makes it).
	cm := pl.ArgoCDValues["configs"].(map[string]any)["cm"].(map[string]any)
	if cm["application.sync.impersonation.enabled"] != "true" || cm["accounts.ladder"] != "login" {
		t.Errorf("argocd-cm values = %v", cm)
	}
	extra := pl.ArgoCDValues["configs"].(map[string]any)["secret"].(map[string]any)["extra"].(map[string]any)
	if extra["accounts.ladder.password"] != "$2a$10$hash" {
		t.Errorf("viewer password hash = %v", extra)
	}
	if _, ok := pl.ArgoCDValues["extraObjects"]; ok {
		t.Error("Argo CD's values declare objects")
	}
	app := find(t, pl, "Application", "argocd")
	if p, _, _ := unstructured.NestedString(app.Object, "spec", "project"); p != Project {
		t.Errorf("argocd project = %q", p)
	}
	if v, _, _ := unstructured.NestedMap(app.Object, "spec", "source", "helm", "valuesObject"); v["configs"] == nil {
		t.Error("Application argocd has not Argo CD's values")
	}
	if prune, _, _ := unstructured.NestedBool(app.Object, "spec", "syncPolicy", "automated", "prune"); prune {
		t.Error("Argo CD prunes its own objects")
	}
	// The project apps syncs as the deployer, never into the platform's
	// namespaces, and takes only Namespace of cluster-wide kinds.
	apps := find(t, pl, "AppProject", "apps")
	sa, _, _ := unstructured.NestedSlice(apps.Object, "spec", "destinationServiceAccounts")
	if len(sa) != 1 || sa[0].(map[string]any)["defaultServiceAccount"] != "argocd:ladder-apps-deployer" {
		t.Errorf("destinationServiceAccounts = %v", sa)
	}
	dest, _, _ := unstructured.NestedSlice(apps.Object, "spec", "destinations")
	var ns []string
	for _, d := range dest {
		ns = append(ns, d.(map[string]any)["namespace"].(string))
	}
	for _, n := range []string{"!argocd", "!ladder", "!kube-*", "*"} {
		if !slices.Contains(ns, n) {
			t.Errorf("destinations %v lack %s", ns, n)
		}
	}
	// With impersonation on, every project that syncs names a ServiceAccount:
	// without one, Argo CD cannot sync even itself.
	plat := find(t, pl, "AppProject", Project)
	if sa, _, _ := unstructured.NestedSlice(plat.Object, "spec", "destinationServiceAccounts"); len(sa) != 1 ||
		sa[0].(map[string]any)["defaultServiceAccount"] != "argocd:ladder-platform-deployer" {
		t.Errorf("platform destinationServiceAccounts = %v", sa)
	}
	// default may deploy nothing.
	def := find(t, pl, "AppProject", "default")
	if r, _, _ := unstructured.NestedSlice(def.Object, "spec", "sourceRepos"); len(r) != 0 {
		t.Errorf("default sourceRepos = %v", r)
	}
	if d, found, _ := unstructured.NestedSlice(def.Object, "spec", "destinations"); !found || len(d) != 0 {
		t.Errorf("default destinations = %v, %v", d, found)
	}
	// The ApplicationSet labels the namespaces of apps.
	set := find(t, pl, "ApplicationSet", "apps")
	if l, _, _ := unstructured.NestedStringMap(set.Object, "spec", "template", "spec", "syncPolicy", "managedNamespaceMetadata", "labels"); l[AppsLabel] != "true" {
		t.Errorf("managed namespace labels = %v", l)
	}
	if name, _, _ := unstructured.NestedString(set.Object, "spec", "template", "metadata", "name"); name != "{{.name}}" {
		t.Errorf("ApplicationSet template name = %q", name)
	}
	if pl.Ingress == nil || pl.Ingress.GetName() != Ingress || len(pl.Ingress.GetAnnotations()) > 0 {
		t.Errorf("Ingress = %v", pl.Ingress)
	}
	again, _ := Render(params())
	if again.Hash != pl.Hash || len(pl.Hash) != 64 {
		t.Errorf("hash %q, again %q", pl.Hash, again.Hash)
	}
}

func TestRenderTraefikAndMetricsServer(t *testing.T) {
	p := params()
	p.Traefik, p.MetricsServer = true, true
	pl, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"argocd", "traefik", "cert-manager", "metrics-server"} {
		find(t, pl, "Application", name)
	}
	cert := find(t, pl, "Application", "cert-manager")
	if opts, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "syncOptions"); opts != nil {
		t.Errorf("syncOptions at the wrong level: %v", opts)
	}
	if opts, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "syncPolicy", "syncOptions"); !slices.Contains(opts, "ServerSideApply=true") {
		t.Errorf("cert-manager syncOptions = %v", opts)
	}
	if pl.Ingress.GetAnnotations()["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("Ingress annotations = %v", pl.Ingress.GetAnnotations())
	}
	plain, _ := Render(params())
	if plain.Hash == pl.Hash {
		t.Error("hash does not change with the components")
	}
}

func TestOverrides(t *testing.T) {
	p := params()
	p.Overrides = map[string]string{"argocd.yaml": "controller:\n  resources:\n    limits: {memory: 1Gi}\nserver:\n  env: []\n"}
	pl, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	c := pl.ArgoCDValues["controller"].(map[string]any)["resources"].(map[string]any)
	if c["limits"].(map[string]any)["memory"] != "1Gi" || c["requests"] == nil {
		t.Errorf("controller resources = %v", c)
	}
	if env := pl.ArgoCDValues["server"].(map[string]any)["env"].([]any); len(env) != 0 {
		t.Errorf("lists are not replaced: %v", env)
	}
	plain, _ := Render(params())
	if plain.Hash == pl.Hash {
		t.Error("hash does not change with overrides")
	}
	p.Overrides = map[string]string{"argocd.yaml": "controller: ["}
	if _, err := Render(p); err == nil || !strings.Contains(err.Error(), OverridesConfigMap) {
		t.Errorf("bad override: %v", err)
	}
}

func TestRenderRefusesMissingParams(t *testing.T) {
	p := params()
	p.Viewer = Viewer{}
	if _, err := Render(p); err == nil {
		t.Error("rendered without the viewer's password")
	}
}

func TestSSLIPHost(t *testing.T) {
	for addrs, want := range map[string]string{
		"203.0.113.7":                "argocd.203-0-113-7.sslip.io",
		"10.0.0.5,203.0.113.7":       "argocd.203-0-113-7.sslip.io",
		"10.0.0.5,100.64.1.1":        "",
		"lb.example.com,192.168.1.1": "",
		"2001:db8::1":                "",
		"":                           "",
	} {
		if got := SSLIPHost(strings.Split(addrs, ",")); got != want {
			t.Errorf("%s: %q, want %q", addrs, got, want)
		}
	}
}

func ingress(host, ip string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: Ingress, Namespace: "argocd"},
		Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: host}}},
	}
	if ip != "" {
		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: ip}}
	}
	return ing
}

func TestSetHost(t *testing.T) {
	ctx := context.Background()
	kc := fake.NewClientset(ingress("", ""))
	k := &kube.Clients{Kube: kc}
	if done, err := SetHost(ctx, k, "argocd", quiet); done || err != nil {
		t.Fatalf("without an address: done=%v err=%v", done, err)
	}
	kc = fake.NewClientset(ingress("", "203.0.113.7"))
	k = &kube.Clients{Kube: kc}
	if done, err := SetHost(ctx, k, "argocd", quiet); !done || err != nil {
		t.Fatalf("with an address: done=%v err=%v", done, err)
	}
	ing, _ := kc.NetworkingV1().Ingresses("argocd").Get(ctx, Ingress, metav1.GetOptions{})
	if ing.Spec.Rules[0].Host != "argocd.203-0-113-7.sslip.io" || len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != TLSSecret {
		t.Errorf("Ingress = %+v", ing.Spec)
	}
	// A host people chose stays.
	kc = fake.NewClientset(ingress("argocd.example.com", "203.0.113.7"))
	k = &kube.Clients{Kube: kc}
	if done, err := SetHost(ctx, k, "argocd", quiet); !done || err != nil {
		t.Fatalf("with a host: done=%v err=%v", done, err)
	}
	ing, _ = kc.NetworkingV1().Ingresses("argocd").Get(ctx, Ingress, metav1.GetOptions{})
	if ing.Spec.Rules[0].Host != "argocd.example.com" || len(ing.Spec.TLS) != 0 {
		t.Errorf("Ingress changed: %+v", ing.Spec)
	}
}

func testApp(name, project string, multi bool, labels map[string]any, ns string) *unstructured.Unstructured {
	spec := map[string]any{"project": project, "destination": map[string]any{"namespace": ns}}
	if multi {
		spec["sources"] = []any{map[string]any{"repoURL": "x"}}
	}
	md := map[string]any{"name": name, "namespace": "argocd", "finalizers": []any{"resources-finalizer.argocd.argoproj.io"}}
	if labels != nil {
		md["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": md, "spec": spec,
	}}
}

// A cluster set up from git loses root, appsets and the platform's
// Applications that took values from git; apps stay, and their namespaces
// get the label.
func TestMigrateAndLabel(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{kube.ApplicationGVR: "ApplicationList", ingressGVR: "IngressList"},
		testApp("root", "default", false, nil, "argocd"),
		testApp("appsets", "default", false, nil, "argocd"),
		testApp("argocd", "default", true, nil, "argocd"),
		testApp("traefik", Project, false, nil, "traefik"),
		testApp("shop", "apps", false, map[string]any{"ladder.dev/managed": "true"}, "shop"),
		testApp("tools", "apps", false, map[string]any{"ladder.dev/managed": "true"}, "kube-system"),
	)
	kc := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	)
	k := &kube.Clients{Kube: kc, Dynamic: dc}
	apps := dc.Resource(kube.ApplicationGVR).Namespace("argocd")
	if err := migrate(ctx, k, apps, "argocd", []string{"argocd", "traefik"}, quiet); err != nil {
		t.Fatal(err)
	}
	list, _ := apps.List(ctx, metav1.ListOptions{})
	var left []string
	for _, u := range list.Items {
		left = append(left, u.GetName())
	}
	slices.Sort(left)
	if !slices.Equal(left, []string{"shop", "tools", "traefik"}) {
		t.Errorf("Applications left: %v", left)
	}
	if err := labelAppNamespaces(ctx, k, apps, params(), quiet); err != nil {
		t.Fatal(err)
	}
	shop, _ := kc.CoreV1().Namespaces().Get(ctx, "shop", metav1.GetOptions{})
	sys, _ := kc.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if shop.Labels[AppsLabel] != "true" || sys.Labels[AppsLabel] != "" {
		t.Errorf("labels: shop %v, kube-system %v", shop.Labels, sys.Labels)
	}
}

func TestMerge(t *testing.T) {
	got := Merge(map[string]any{"a": map[string]any{"b": 1, "c": 2}, "l": []any{1}}, map[string]any{"a": map[string]any{"c": 3}, "l": []any{}})
	if got["a"].(map[string]any)["b"] != 1 || got["a"].(map[string]any)["c"] != 3 || len(got["l"].([]any)) != 0 {
		t.Errorf("Merge = %v", got)
	}
}

// A platform Application whose last sync failed gets a sync of its own; one
// that synced, or is syncing, is left alone.
func TestResync(t *testing.T) {
	ctx := context.Background()
	failed := testApp("argocd", Project, false, nil, "argocd")
	failed.Object["status"] = map[string]any{"operationState": map[string]any{"phase": "Failed"}}
	_ = unstructured.SetNestedMap(failed.Object, map[string]any{"limit": int64(5)}, "spec", "syncPolicy", "retry")
	ok := testApp("traefik", Project, false, nil, "traefik")
	ok.Object["status"] = map[string]any{"operationState": map[string]any{"phase": "Succeeded"}}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{kube.ApplicationGVR: "ApplicationList"}, failed, ok)
	apps := dc.Resource(kube.ApplicationGVR).Namespace("argocd")
	for _, name := range []string{"argocd", "traefik", "missing"} {
		if err := resync(ctx, apps, name, quiet); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := apps.Get(ctx, "argocd", metav1.GetOptions{})
	if limit, _, _ := unstructured.NestedInt64(got.Object, "operation", "retry", "limit"); limit != 5 {
		t.Errorf("argocd operation = %v", got.Object["operation"])
	}
	got, _ = apps.Get(ctx, "traefik", metav1.GetOptions{})
	if _, ok := got.Object["operation"]; ok {
		t.Error("a synced Application syncs again")
	}
}
