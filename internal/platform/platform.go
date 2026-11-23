// Package platform renders and applies what the agent sets up around the
// apps (spec §5.4, §5.6, §5.8): Argo CD's Helm values, the Applications of
// Argo CD, Traefik, cert-manager and Metrics Server, the AppProjects and the
// ApplicationSet of apps, the ServiceAccount apps sync as, and the policies
// that keep apps in their namespaces.
//
// They come from the agent's image, not from git. The repository the cloud
// writes holds only apps, which Argo CD applies with the rights of the
// project apps: a compromised cloud can change apps, but not the platform
// nor the rules that limit apps.
package platform

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

//go:embed files
var files embed.FS

const (
	// Project is the AppProject of the platform's Applications.
	Project = "platform"
	// Deployer is the ServiceAccount, in Argo CD's namespace, that Argo CD
	// syncs apps as.
	Deployer = "ladder-apps-deployer"
	// PlatformDeployer is the ServiceAccount, in Argo CD's namespace, that
	// Argo CD syncs the platform's charts as.
	PlatformDeployer = "ladder-platform-deployer"
	// AppsLabel marks the namespaces of apps. Ladder's policies hold there.
	AppsLabel = "ladder.dev/apps"
	// Ingress is Argo CD's Ingress.
	Ingress = "argocd-server"
	// OverridesConfigMap, in the agent's namespace, holds Helm values people
	// set over Ladder's: one key per component, such as traefik.yaml.
	OverridesConfigMap = "ladder-platform"

	inCluster = "https://kubernetes.default.svc"
)

// Viewer is Argo CD's read-only account ladder, which Ladder shows people.
type Viewer struct {
	Password      string
	PasswordHash  string
	PasswordMtime string
}

// Params fill the templates. They come from the setup plan, checked by the
// agent, and from the cluster.
type Params struct {
	ArgoCDNamespace    string
	ArgoCDChartRepo    string
	ArgoCDChartVersion string
	ArgoCDVersion      string
	AgentNamespace     string
	RepoURL            string
	// Exposure is "ingress" (the UI on path /argocd) or "port-forward".
	Exposure string
	// Traefik adds Traefik and cert-manager (spec §5.12).
	Traefik bool
	// MetricsServer adds Metrics Server, for a cluster without a Metrics API.
	MetricsServer bool
	Viewer        Viewer
	// Overrides are Helm values people set per component, by its file name
	// such as traefik.yaml, merged over Ladder's.
	Overrides map[string]string
}

// Component is one Helm chart of the platform, which an Application of the
// project platform installs.
type Component struct {
	Name, Namespace, Repo, Chart, Version, Note string
}

// Platform is what Render makes.
type Platform struct {
	// ArgoCDValues are the Helm values setup installs Argo CD with, the same
	// as the Application argocd has.
	ArgoCDValues map[string]any
	// Objects are applied in order, server-side, by the agent.
	Objects []*unstructured.Unstructured
	// Ingress is made only when Argo CD has none.
	Ingress *unstructured.Unstructured
	// Hash identifies Objects: the agent applies them again when it changes.
	Hash string
}

// Components lists the platform's charts for p, Argo CD first.
func Components(p Params) []Component {
	cs := []Component{{
		Name: "argocd", Namespace: p.ArgoCDNamespace, Repo: p.ArgoCDChartRepo, Chart: "argo-cd",
		Version: p.ArgoCDChartVersion, Note: "Argo CD " + p.ArgoCDVersion,
	}}
	if p.Traefik {
		cs = append(cs,
			Component{Name: "traefik", Namespace: "traefik", Repo: "https://traefik.github.io/charts", Chart: "traefik", Version: "41.6.1", Note: "Traefik 3.7.13"},
			Component{Name: "cert-manager", Namespace: "cert-manager", Repo: "https://charts.jetstack.io", Chart: "cert-manager", Version: "v1.21.2", Note: "cert-manager 1.21.2"},
		)
	}
	if p.MetricsServer {
		cs = append(cs, Component{Name: "metrics-server", Namespace: "kube-system", Repo: "https://kubernetes-sigs.github.io/metrics-server/", Chart: "metrics-server", Version: "3.14.0", Note: "Metrics Server 0.9.0"})
	}
	return cs
}

// repos are the Helm repositories the project platform takes, whichever
// components a cluster has.
func repos(p Params) []string {
	return []string{p.ArgoCDChartRepo, "https://traefik.github.io/charts", "https://charts.jetstack.io", "https://kubernetes-sigs.github.io/metrics-server/"}
}

// Render makes the platform for p.
func Render(p Params) (*Platform, error) {
	if p.ArgoCDNamespace == "" || p.AgentNamespace == "" || p.RepoURL == "" || p.ArgoCDChartRepo == "" || p.ArgoCDChartVersion == "" {
		return nil, fmt.Errorf("platform: namespaces, repository and Argo CD chart are required")
	}
	if p.Viewer.PasswordHash == "" {
		return nil, fmt.Errorf("platform: the read-only account has no password")
	}
	data := struct {
		Params
		PlatformRepos                         []string
		Deployer, PlatformDeployer, AppsLabel string
	}{p, repos(p), Deployer, PlatformDeployer, AppsLabel}

	out := &Platform{}
	for _, c := range Components(p) {
		values, err := componentValues(c.Name, data, p.Overrides)
		if err != nil {
			return nil, err
		}
		if c.Name == "argocd" {
			out.ArgoCDValues = values
		}
		out.Objects = append(out.Objects, application(p, c, values))
	}
	objs, err := renderObjects("objects.yaml", data)
	if err != nil {
		return nil, err
	}
	// The projects and the deployer before the Applications that use them.
	out.Objects = append(objs, out.Objects...)
	if p.Exposure == "ingress" {
		ing, err := renderObjects("ingress.yaml", data)
		if err != nil {
			return nil, err
		}
		out.Ingress = ing[0]
	}
	b, err := json.Marshal(out.Objects)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	out.Hash = hex.EncodeToString(sum[:])
	return out, nil
}

// componentValues renders the component's values and merges people's
// overrides over them.
func componentValues(name string, data any, overrides map[string]string) (map[string]any, error) {
	file := name + ".yaml"
	src, err := render(file, data)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if err := yaml.Unmarshal(src, &values); err != nil {
		return nil, fmt.Errorf("platform %s: %w", file, err)
	}
	if o := strings.TrimSpace(overrides[file]); o != "" {
		var over map[string]any
		if err := yaml.Unmarshal([]byte(o), &over); err != nil {
			return nil, fmt.Errorf("ConfigMap %s, key %s: %w", OverridesConfigMap, file, err)
		}
		values = Merge(values, over)
	}
	return values, nil
}

// Merge returns base with over merged in: maps key by key, anything else,
// lists included, replaced.
func Merge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		bm, ok1 := out[k].(map[string]any)
		om, ok2 := v.(map[string]any)
		if ok1 && ok2 {
			out[k] = Merge(bm, om)
			continue
		}
		out[k] = v
	}
	return out
}

func application(p Params, c Component, values map[string]any) *unstructured.Unstructured {
	automated := map[string]any{"prune": true, "selfHeal": true}
	policy := map[string]any{"automated": automated}
	switch c.Name {
	case "argocd":
		// Argo CD never deletes its own objects.
		automated["prune"] = false
	case "traefik":
		policy["syncOptions"] = []any{"CreateNamespace=true"}
	case "cert-manager":
		// Some of cert-manager's CRDs are larger than the annotation a
		// client-side apply keeps. The issuer waits for cert-manager's
		// webhook, which may take a moment to answer after its pod is ready.
		policy["syncOptions"] = []any{"CreateNamespace=true", "ServerSideApply=true"}
		policy["retry"] = retry(10, "3m")
	case "metrics-server":
		policy["retry"] = retry(5, "1m")
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      c.Name,
			"namespace": p.ArgoCDNamespace,
			"labels":    map[string]any{"app.kubernetes.io/managed-by": "ladder"},
		},
		"spec": map[string]any{
			"project": Project,
			"source": map[string]any{
				"repoURL":        c.Repo,
				"chart":          c.Chart,
				"targetRevision": c.Version,
				"helm":           map[string]any{"releaseName": c.Name, "valuesObject": values},
			},
			"destination": map[string]any{"server": inCluster, "namespace": c.Namespace},
			"syncPolicy":  policy,
		},
	}}
	return u
}

func retry(limit int64, max string) map[string]any {
	return map[string]any{"limit": limit, "backoff": map[string]any{"duration": "10s", "factor": int64(2), "maxDuration": max}}
}

func render(file string, data any) ([]byte, error) {
	src, err := files.ReadFile("files/" + file)
	if err != nil {
		return nil, err
	}
	t, err := template.New(file).Delims("[[", "]]").Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderObjects renders a file of YAML documents.
func renderObjects(file string, data any) ([]*unstructured.Unstructured, error) {
	src, err := render(file, data)
	if err != nil {
		return nil, err
	}
	var out []*unstructured.Unstructured
	for _, doc := range bytes.Split(src, []byte("\n---\n")) {
		obj := map[string]any{}
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			return nil, fmt.Errorf("platform %s: %w", file, err)
		}
		if len(obj) == 0 {
			continue
		}
		out = append(out, &unstructured.Unstructured{Object: obj})
	}
	return out, nil
}
