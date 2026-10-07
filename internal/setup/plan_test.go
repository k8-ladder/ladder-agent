package setup

import (
	"context"
	"testing"

	"github.com/ladder/ladder-agent/internal/config"
	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func ladderPlan() protocol.SetupPlan {
	return protocol.SetupPlan{
		Provider: protocol.ProviderLadder,
		Owner:    "acme",
		Repo:     "prod",
		RepoURL:  "https://ladder.example.com/git/acme/prod.git",
		Files: map[string]string{
			fileLadder: "branch: main\n", fileRootApp: "kind: Application\n", fileArgoCDValues: "configs: {}\n",
		},
		Credentials: &protocol.RepoCredentials{Username: "ladder-c-1", Password: "token-1"},
	}
}

func TestPrepareLadder(t *testing.T) {
	e := &Engine{cfg: config.Config{ArgoCDNamespace: "argocd", ArgoCDVersion: "3.5.3", ArgoCDChartVersion: "10.9.6"}}
	p, err := e.prepare(context.Background(), ladderPlan(), true)
	if err != nil {
		t.Fatal(err)
	}
	if p.WriteMode != protocol.WriteModeCommit || p.ArgoCD.Namespace != "argocd" || p.ArgoCD.Exposure != "ingress" || p.ArgoCD.ChartVersion != "10.9.6" {
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
		"no starter file": func(p *protocol.SetupPlan) { delete(p.Files, fileRootApp) },
		"old local":       func(p *protocol.SetupPlan) { p.Provider = "local" },
	} {
		in := ladderPlan()
		change(&in)
		if _, err := e.prepare(context.Background(), in, true); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// setup.plan needs no credentials and ignores the files.
	in := ladderPlan()
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
