package setup

import (
	"context"
	"testing"

	"github.com/ladder/ladder-agent/internal/config"
	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func ladderPlan() protocol.SetupPlan {
	return protocol.SetupPlan{
		Provider: protocol.ProviderLadder,
		Owner:    "acme",
		Repo:     "prod",
		RepoURL:  "https://ladder.example.com/git/acme/prod.git",
		Credentials: &protocol.RepoCredentials{Username: "ladder-c-1", Password: "token-1"},
	}
}

func TestPlanMetricsServer(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "registered but unhealthy"}[present], func(t *testing.T) {
			var objs []runtime.Object
			if present {
				objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService",
					"metadata": map[string]any{"name": "v1beta1.metrics.k8s.io"},
					"status":   map[string]any{"conditions": []any{map[string]any{"type": "Available", "status": "False"}}},
				}})
			}
			dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...)
			e := &Engine{cfg: config.Config{ArgoCDNamespace: "argocd"}, k: &kube.Clients{Dynamic: dc}}
			p, err := e.Plan(t.Context(), ladderPlan())
			if err != nil {
				t.Fatal(err)
			}
			if p.MetricsServerPresent == nil || *p.MetricsServerPresent != present {
				t.Fatalf("metrics presence = %v, want %v", p.MetricsServerPresent, present)
			}
			for _, action := range dc.Actions() {
				if action.GetVerb() != "get" {
					t.Errorf("setup.plan changed cluster: %+v", action)
				}
			}
		})
	}

	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dc.PrependReactor("get", "apiservices", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(kube.APIServiceGVR.GroupResource(), "v1beta1.metrics.k8s.io", nil)
	})
	e := &Engine{cfg: config.Config{ArgoCDNamespace: "argocd"}, k: &kube.Clients{Dynamic: dc}}
	if _, err := e.Plan(t.Context(), ladderPlan()); !apierrors.IsForbidden(err) {
		t.Fatalf("refused detection = %v, want forbidden", err)
	}
}

func TestGitHubDeployKey(t *testing.T) {
	ctx := context.Background()
	e := &Engine{cfg: config.Config{ArgoCDNamespace: "argocd"}, k: &kube.Clients{Kube: fake.NewClientset()}}
	p := ladderPlan()
	p.Provider = protocol.ProviderGitHub
	p.RepoURL = "ssh://git@github.com/acme/prod.git"
	p.Credentials = &protocol.RepoCredentials{SSHPrivateKey: "private-key"}
	prepared, err := e.prepare(ctx, p, true)
	if err != nil {
		t.Fatal(err)
	}
	s := e.stepRepoSecret(prepared)
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	sec, err := e.k.Kube.CoreV1().Secrets("argocd").Get(ctx, repoSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(sec.Data["sshPrivateKey"]) != "private-key" || string(sec.Data["url"]) != p.RepoURL || len(sec.Data) != 3 {
		t.Fatalf("repository Secret = %+v", sec.Data)
	}
	if done, err := s.done(ctx); err != nil || !done {
		t.Fatalf("read-only key Secret not done: %v %v", done, err)
	}
	p.Credentials.SSHPrivateKey = "rotated-key"
	if done, _ := s.done(ctx); done {
		t.Fatal("old deploy key counted as done")
	}
	p.RepoURL = "ssh://git@evil.example/acme/prod.git"
	if _, err := e.prepare(ctx, p, true); err == nil {
		t.Fatal("accepted deploy key for another host")
	}
	p.RepoURL = "https://github.com/acme/prod.git"
	if _, err := e.prepare(ctx, p, true); err == nil {
		t.Fatal("accepted HTTPS URL with SSH credentials")
	}
}

func TestPrepareLadder(t *testing.T) {
	e := &Engine{cfg: config.Config{Namespace: "ladder", ArgoCDNamespace: "argocd", ArgoCDVersion: "3.5.3", ArgoCDChartVersion: "10.9.6"}}
	in := ladderPlan()
	// A cloud cannot pick Argo CD's version, nor have the agent take the
	// Metrics Server decision from it; old clouds' files are dropped.
	in.ArgoCD.ChartVersion, in.ArgoCD.Version = "1.0.0", "1.0"
	yes := true
	in.MetricsServer = &yes
	in.Files = map[string]string{"bootstrap/root.yaml": "kind: Application\n"}
	p, err := e.prepare(context.Background(), in, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.WriteMode != protocol.WriteModeCommit || p.ArgoCD.Namespace != "argocd" || p.ArgoCD.Exposure != "ingress" ||
		p.ArgoCD.ChartVersion != "10.9.6" || p.ArgoCD.Version != "3.5.3" || p.MetricsServer != nil || p.Files != nil {
		t.Errorf("defaults not filled in: %+v", p)
	}
	if p.RepoURL != "https://ladder.example.com/git/acme/prod.git" {
		t.Errorf("repoURL = %q, want the cloud's", p.RepoURL)
	}

	for name, change := range map[string]func(*protocol.SetupPlan){
		"no owner":        func(p *protocol.SetupPlan) { p.Owner = "" },
		"bad repo":        func(p *protocol.SetupPlan) { p.Repo = "a/b" },
		"no repoURL":      func(p *protocol.SetupPlan) { p.RepoURL = "" },
		"ssh repoURL":     func(p *protocol.SetupPlan) { p.RepoURL = "git@ladder.example.com:acme/prod.git" },
		"no credentials":  func(p *protocol.SetupPlan) { p.Credentials = nil },
		"no password":     func(p *protocol.SetupPlan) { p.Credentials.Password = "" },
		"agent namespace": func(p *protocol.SetupPlan) { p.ArgoCD.Namespace = "ladder" },
		"kube namespace":  func(p *protocol.SetupPlan) { p.ArgoCD.Namespace = "kube-system" },
		"old local":       func(p *protocol.SetupPlan) { p.Provider = "local" },
	} {
		in := ladderPlan()
		change(&in)
		if _, err := e.prepare(context.Background(), in, true); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// setup.plan needs no credentials and ignores the files.
	in = ladderPlan()
	in.Credentials = nil
	if p, err := e.prepare(context.Background(), in, false); err != nil || p.Files != nil {
		t.Errorf("plan without credentials: %v, files %v", err, p)
	}
}

func TestRepoSecret(t *testing.T) {
	ctx := context.Background()
	e := &Engine{k: &kube.Clients{Kube: fake.NewClientset()}}
	p := ladderPlan()
	p.ArgoCD.Namespace = "argocd"
	s := e.stepRepoSecret(&p)

	if done, err := s.done(ctx); err != nil || done {
		t.Fatalf("done before the Secret exists = %v, %v", done, err)
	}
	if err := s.run(ctx); err != nil {
		t.Fatal(err)
	}
	sec, err := e.k.Kube.CoreV1().Secrets("argocd").Get(ctx, repoSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"type": "git", "url": p.RepoURL, "username": "ladder-c-1", "password": "token-1"}
	for k, v := range want {
		if string(sec.Data[k]) != v {
			t.Errorf("%s = %q, want %q", k, sec.Data[k], v)
		}
	}
	if sec.Labels["argocd.argoproj.io/secret-type"] != "repository" {
		t.Errorf("labels = %v", sec.Labels)
	}
	if done, err := s.done(ctx); err != nil || !done {
		t.Errorf("done after run = %v, %v", done, err)
	}

	// Setup applied again with a new token: the step runs again.
	p.Credentials = &protocol.RepoCredentials{Username: "ladder-c-1", Password: "token-2"}
	if done, _ := s.done(ctx); done {
		t.Error("a Secret with the old token counts as done")
	}
}
