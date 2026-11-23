package setup

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ladder/ladder-agent/internal/platform"
	"github.com/ladder/ladder-agent/internal/protocol"
)

var (
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

func badRequest(format string, args ...any) error {
	return protocol.Errorf(protocol.CodeBadRequest, format, args...)
}

// prepare validates the request and fills in defaults. With forApply it also
// requires the repository's credentials. Files from older clouds are dropped:
// the platform comes from the agent (package platform).
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
	// The agent's own Argo CD version, whatever the cloud asks: the platform
	// comes from the agent's image, and a cloud cannot pick an older one.
	a.Version, a.ChartVersion = e.cfg.ArgoCDVersion, e.cfg.ArgoCDChartVersion
	if a.Namespace == "" {
		a.Namespace = e.cfg.ArgoCDNamespace
	}
	if a.Namespace == e.cfg.Namespace || strings.HasPrefix(a.Namespace, "kube-") {
		return nil, badRequest("argocd.namespace %q is the agent's or Kubernetes' own", a.Namespace)
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

	p.Files = nil
	p.MetricsServer = nil
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
		add("Ingress", ns, platform.Ingress, "Argo CD UI on path /argocd")
	}
	add("Secret", ns, repoSecretName, "repository credentials for Argo CD, not in git")
	add("AppProject", ns, "default", "locked: may deploy nothing")
	add("AppProject", ns, platform.Project, "the charts below, from their Helm repositories only")
	add("AppProject", ns, "apps", "apps from the repository, synced as "+platform.Deployer)
	params := e.planParams(p)
	for _, c := range platform.Components(params) {
		add("Application", ns, c.Name, c.Chart+" chart "+c.Version+", "+c.Note)
	}
	add("ApplicationSet", ns, "apps", "one Application per apps/<name>/app.yaml")
	add("ServiceAccount", ns, platform.Deployer, "apps sync as it: admin in the namespaces of apps")
	add("ValidatingAdmissionPolicy", "", "ladder-apps-baseline", "pods in the namespaces of apps meet the baseline Pod Security Standard")
	add("ValidatingAdmissionPolicy", "", "ladder-apps-namespaces", "apps go only into namespaces labelled "+platform.AppsLabel)
	add("Secret", e.cfg.Namespace, platform.ViewerSecret, "password of Argo CD's read-only account "+platform.ViewerUser)
	add("Secret", e.cfg.Namespace, ConnectionSecret, "provider, repository URL, Argo CD settings")
	return objs
}

// planParams are the platform's settings in plan p, without the read-only
// account's password and people's overrides.
func (e *Engine) planParams(p *protocol.SetupPlan) platform.Params {
	return platform.Params{
		ArgoCDNamespace:    p.ArgoCD.Namespace,
		ArgoCDChartRepo:    e.cfg.ArgoCDChartRepo,
		ArgoCDChartVersion: e.cfg.ArgoCDChartVersion,
		ArgoCDVersion:      e.cfg.ArgoCDVersion,
		AgentNamespace:     e.cfg.Namespace,
		RepoURL:            p.RepoURL,
		Exposure:           p.ArgoCD.Exposure,
		Traefik:            p.Traefik,
		MetricsServer:      p.MetricsServer != nil && *p.MetricsServer,
	}
}

// render makes the platform of plan p: it reads people's overrides and makes
// the read-only account's password the first time.
func (e *Engine) render(ctx context.Context, p *protocol.SetupPlan) (platform.Params, *platform.Platform, error) {
	params := e.planParams(p)
	v, err := platform.EnsureViewer(ctx, e.k, e.cfg.Namespace, time.Now())
	if err != nil {
		return params, nil, fmt.Errorf("Argo CD's read-only account: %w", err)
	}
	params.Viewer = v
	if params.Overrides, err = platform.Overrides(ctx, e.k, e.cfg.Namespace); err != nil {
		return params, nil, err
	}
	pl, err := platform.Render(params)
	return params, pl, err
}

// legacyFlags fills in Traefik and Metrics Server for a plan saved before the
// agent made the platform, from the files the cloud sent then.
func legacyFlags(p *protocol.SetupPlan) {
	if p.MetricsServer == nil && len(p.Files) > 0 {
		m := p.Files["bootstrap/metrics-server.yaml"] != ""
		p.MetricsServer = &m
	}
	if !p.Traefik && p.Files["bootstrap/traefik.yaml"] != "" {
		p.Traefik = true
	}
}
