package setup

import (
	"context"
	"fmt"
	"net/url"
	"regexp"

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
	case protocol.ProviderLadder:
		if !repoNameRe.MatchString(p.Owner) || !repoNameRe.MatchString(p.Repo) {
			return nil, badRequest("owner and a valid repo are required")
		}
		if u, err := url.Parse(p.RepoURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, badRequest("repoURL %q is not an http or https URL", p.RepoURL)
		}
		if forApply {
			if c := p.Credentials; c == nil || c.Username == "" || c.Password == "" {
				return nil, badRequest("credentials with a username and a password are required")
			}
		}
	case protocol.ProviderGitHub:
		if p.Owner == "" || !repoNameRe.MatchString(p.Repo) {
			return nil, badRequest("owner and a valid repo are required for GitHub")
		}
		if p.RepoURL == "" {
			p.RepoURL = fmt.Sprintf("https://github.com/%s/%s.git", p.Owner, p.Repo)
		}
		if p.Credentials != nil && p.Credentials.SSHPrivateKey != "" {
			expected := fmt.Sprintf("ssh://git@github.com/%s/%s.git", p.Owner, p.Repo)
			if p.RepoURL != expected {
				return nil, badRequest("SSH deploy keys require repoURL %q", expected)
			}
		}
		if forApply {
			c := p.Credentials
			if c == nil || c.SSHPrivateKey == "" && (c.GitHubAppID == 0 || c.GitHubAppInstallationID == 0 || c.GitHubAppPrivateKey == "") {
				return nil, badRequest("credentials with a read-only SSH deploy key or GitHub App are required")
			}
		}
	default:
		return nil, badRequest("provider must be %q or %q", protocol.ProviderLadder, protocol.ProviderGitHub)
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
	add("HelmRelease", ns, argoRelease, "argo-cd chart "+p.ArgoCD.ChartVersion+", Argo CD "+p.ArgoCD.Version)
	if p.ArgoCD.Exposure == "ingress" {
		add("Ingress", ns, argoIngress, "Argo CD UI on path /argocd, from "+fileArgoCDValues)
	}
	add("Secret", ns, repoSecretName, "repository credentials for Argo CD, not in git")
	add("Application", ns, "root", "from "+fileRootApp)
	add("Secret", e.cfg.Namespace, ConnectionSecret, "provider, repository URL, Argo CD settings")
	return objs
}
