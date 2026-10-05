package setup

import (
	"context"
	"fmt"
	"regexp"

	"github.com/ladder/ladder-agent/internal/gitea"
	"github.com/ladder/ladder-agent/internal/protocol"
	"sigs.k8s.io/yaml"
)

var (
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

const (
	fileLadder       = "ladder.yaml"
	fileRootApp      = "bootstrap/root.yaml"
	fileArgoCDValues = "platform/argocd/values.yaml"
)

func badRequest(format string, args ...any) error {
	return protocol.Errorf(protocol.CodeBadRequest, format, args...)
}

// prepare validates the request and fills in defaults. With forApply it also
// requires the starter files.
func (e *Engine) prepare(ctx context.Context, in protocol.SetupPlan, forApply bool) (*protocol.SetupPlan, error) {
	p := in
	if p.WriteMode == "" {
		p.WriteMode = protocol.WriteModeCommit
	}
	if p.WriteMode != protocol.WriteModePR && p.WriteMode != protocol.WriteModeCommit {
		return nil, badRequest("writeMode must be %q or %q", protocol.WriteModePR, protocol.WriteModeCommit)
	}

	switch p.Provider {
	case protocol.ProviderLocal:
		p.Owner = gitea.Owner
		if p.Repo == "" {
			p.Repo = "deploy"
		}
		if !repoNameRe.MatchString(p.Repo) {
			return nil, badRequest("repo %q is not a valid repository name", p.Repo)
		}
		p.RepoURL = gitea.RepoURL(p.Repo)
		p.Credentials = nil
	case protocol.ProviderGitHub:
		if p.Owner == "" || !repoNameRe.MatchString(p.Repo) {
			return nil, badRequest("owner and a valid repo are required for GitHub")
		}
		if p.RepoURL == "" {
			p.RepoURL = fmt.Sprintf("https://github.com/%s/%s.git", p.Owner, p.Repo)
		}
		if forApply {
			c := p.Credentials
			if c == nil || c.GitHubAppID == 0 || c.GitHubAppInstallationID == 0 || c.GitHubAppPrivateKey == "" {
				return nil, badRequest("credentials with GitHub App ID, installation ID and private key are required")
			}
		}
	default:
		return nil, badRequest("provider must be %q or %q", protocol.ProviderLocal, protocol.ProviderGitHub)
	}

	a := &p.ArgoCD
	if a.Version == "" {
		a.Version = e.cfg.ArgoCDVersion
	}
	if a.ChartVersion == "" {
		a.ChartVersion = e.cfg.ArgoCDChartVersion
	}
	if a.Namespace == "" {
		a.Namespace = e.cfg.ArgoCDNamespace
	}
	if a.Exposure == "" {
		a.Exposure = "ingress"
	}
	if !dnsLabelRe.MatchString(a.Namespace) {
		return nil, badRequest("argocd.namespace %q is not a DNS-1123 label", a.Namespace)
	}
	if a.Exposure != "ingress" && a.Exposure != "port-forward" {
		return nil, badRequest("argocd.exposure must be \"ingress\" or \"port-forward\"")
	}

	if !forApply {
		p.Files = nil
		return &p, nil
	}
	for _, f := range []string{fileLadder, fileRootApp, fileArgoCDValues} {
		if p.Files[f] == "" {
			return nil, badRequest("files[%q] is required", f)
		}
	}
	var v map[string]any
	if err := yaml.Unmarshal([]byte(p.Files[fileArgoCDValues]), &v); err != nil {
		return nil, badRequest("%s: %v", fileArgoCDValues, err)
	}
	return &p, nil
}

func (e *Engine) clusterObjects(p *protocol.SetupPlan) []protocol.ClusterObject {
	ns := p.ArgoCD.Namespace
	var objs []protocol.ClusterObject
	add := func(kind, namespace, name, note string) {
		objs = append(objs, protocol.ClusterObject{Action: "create", Kind: kind, Namespace: namespace, Name: name, Note: note})
	}
	if p.Provider == protocol.ProviderLocal {
		add("Namespace", "", gitea.Namespace, "in-cluster git server")
		add("Secret", gitea.Namespace, gitea.AdminSecret, "Gitea passwords and API token, not in git")
		add("PersistentVolumeClaim", gitea.Namespace, "gitea-data", "1Gi")
		add("Deployment", gitea.Namespace, gitea.Deployment, "")
		add("Service", gitea.Namespace, gitea.Service, "")
	}
	add("HelmRelease", ns, argoRelease, "argo-cd chart "+p.ArgoCD.ChartVersion+", Argo CD "+p.ArgoCD.Version)
	if p.ArgoCD.Exposure == "ingress" {
		add("Ingress", ns, argoIngress, "Argo CD UI on path /argocd, from "+fileArgoCDValues)
	}
	add("Secret", ns, repoSecretName, "repository credentials for Argo CD, not in git")
	add("Application", ns, "root", "from "+fileRootApp)
	add("Secret", e.cfg.Namespace, ConnectionSecret, "provider, repository URL, credentials reference")
	return objs
}
