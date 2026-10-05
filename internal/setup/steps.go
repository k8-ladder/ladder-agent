package setup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ladder/ladder-agent/internal/gitea"
	"github.com/ladder/ladder-agent/internal/helmx"
	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

const (
	repoSecretName = "repo-deploy"
	argoRelease    = "argocd"
	argoChart      = "argo-cd"
	// argoIngress is declared in platform/argocd/values.yaml (extraObjects).
	argoIngress    = "argocd-server"
	starterMessage = "Ladder: starter repository"
)

// step is one row of the setup progress list. done is the "done when" check:
// a step whose check passes is not run again, which makes setup idempotent.
type step struct {
	id, title, command string
	timeout            time.Duration
	// skip returns a reason when the step does not apply to this plan.
	skip     func() string
	done     func(ctx context.Context) (bool, error)
	run      func(ctx context.Context) error
	diagnose func(ctx context.Context) string
}

func (e *Engine) steps(p *protocol.SetupPlan) []step {
	ns := p.ArgoCD.Namespace
	steps := []step{e.stepCheckCluster(p)}
	if p.Provider == protocol.ProviderLocal {
		steps = append(steps, e.stepGitea(p), e.stepCommitStarter(p))
	}
	return append(steps,
		e.stepArgoInstall(p),
		step{
			id:      "argocd-ready",
			title:   "Wait until Argo CD is ready",
			command: fmt.Sprintf("kubectl -n %s rollout status deploy", ns),
			timeout: 10 * time.Minute,
			done: func(ctx context.Context) (bool, error) {
				ok, _, err := kube.WorkloadsReady(ctx, e.k, ns)
				return ok, err
			},
			run:      func(ctx context.Context) error { return kube.WaitDeploymentsReady(ctx, e.k, ns) },
			diagnose: func(ctx context.Context) string { return kube.Diagnose(ctx, e.k, ns) },
		},
		e.stepRepoSecret(p),
		e.stepRootApp(p),
		e.stepSync(p),
		e.stepSaveConnection(p),
	)
}

// stepCheckCluster stops setup before it changes anything when the cluster
// runs an Argo CD that Ladder did not install, or has no default StorageClass
// for Gitea's volume. An Argo CD in a namespace labelled as Ladder's is from
// an earlier attempt and passes.
func (e *Engine) stepCheckCluster(p *protocol.SetupPlan) step {
	ns := p.ArgoCD.Namespace
	return step{
		id:      "check-cluster",
		title:   "Check the cluster",
		command: "kubectl get crd applications.argoproj.io; kubectl get storageclass",
		timeout: time.Minute,
		run: func(ctx context.Context) error {
			argo, err := e.inspector.ArgoCD(ctx)
			if err != nil {
				return fmt.Errorf("detect Argo CD: %w", err)
			}
			if argo.CRD {
				where := ns
				if argo.Found {
					where = argo.Namespace
				}
				ours, err := e.k.NamespaceHasLabels(ctx, where, managedLabels())
				if err != nil {
					return err
				}
				if !ours {
					found := "the Argo CD CRDs are installed, maybe left from an earlier Argo CD"
					if argo.Found {
						found = fmt.Sprintf("Argo CD %s runs in namespace %s", argo.Version, argo.Namespace)
					}
					return fmt.Errorf("%s. Ladder installs its own Argo CD and needs a cluster without one", found)
				}
			}
			if p.Provider == protocol.ProviderLocal {
				ok, err := e.inspector.HasDefaultStorageClass(ctx)
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("no default StorageClass: Gitea needs one for its volume")
				}
			}
			return nil
		},
	}
}

func (e *Engine) stepGitea(p *protocol.SetupPlan) step {
	return step{
		id:       "gitea",
		title:    "Install Gitea, create users and repository",
		command:  fmt.Sprintf("kubectl apply -n %s -f gitea.yaml  # then Gitea API: users %s, %s; repository %s/%s", gitea.Namespace, gitea.AdminUser, gitea.ReaderUser, gitea.Owner, p.Repo),
		timeout:  5 * time.Minute,
		done:     func(ctx context.Context) (bool, error) { return e.gitea.RepoReady(ctx, p.Repo) },
		run:      func(ctx context.Context) error { return e.gitea.Install(ctx, p.Repo) },
		diagnose: func(ctx context.Context) string { return kube.Diagnose(ctx, e.k, gitea.Namespace) },
	}
}

func (e *Engine) stepCommitStarter(p *protocol.SetupPlan) step {
	return step{
		id:      "commit-starter",
		title:   "Commit starter files",
		command: "git push origin main",
		timeout: 2 * time.Minute,
		done: func(ctx context.Context) (bool, error) {
			c, err := e.gitea.Client(ctx)
			if err != nil {
				return false, err
			}
			return c.FileExists(ctx, gitea.Owner, p.Repo, fileLadder, "main")
		},
		run: func(ctx context.Context) error {
			c, err := e.gitea.Client(ctx)
			if err != nil {
				return err
			}
			return c.CommitFiles(ctx, gitea.Owner, p.Repo, "main", starterMessage, p.Files)
		},
	}
}

func (e *Engine) stepArgoInstall(p *protocol.SetupPlan) step {
	ns := p.ArgoCD.Namespace
	return step{
		id:      "argocd-install",
		title:   "Install Argo CD",
		command: fmt.Sprintf("helm install %s argo/%s -n %s --create-namespace --version %s -f %s", argoRelease, argoChart, ns, p.ArgoCD.ChartVersion, fileArgoCDValues),
		timeout: 10 * time.Minute,
		done: func(ctx context.Context) (bool, error) {
			rel, err := e.helm.Status(ns, argoRelease)
			return rel != nil && rel.Status == "deployed", err
		},
		run: func(ctx context.Context) error {
			rel, err := e.helm.Status(ns, argoRelease)
			if err != nil {
				return err
			}
			if rel != nil {
				// A pending or failed release blocks a new install; fixing it
				// needs a human decision in the POC.
				return fmt.Errorf("release %s is %s; inspect with `helm -n %s history %s`, then uninstall it and retry", argoRelease, rel.Status, ns, argoRelease)
			}
			var values map[string]any
			if err := yaml.Unmarshal([]byte(p.Files[fileArgoCDValues]), &values); err != nil {
				return err
			}
			// The label marks this Argo CD as Ladder's for the cluster check.
			if err := e.k.EnsureNamespaceLabels(ctx, ns, managedLabels()); err != nil {
				return err
			}
			return e.helm.Install(ctx, helmx.InstallOptions{
				ReleaseName: argoRelease,
				Namespace:   ns,
				RepoURL:     e.cfg.ArgoCDChartRepo,
				Chart:       argoChart,
				Version:     p.ArgoCD.ChartVersion,
				Values:      values,
				Timeout:     10 * time.Minute,
			})
		},
		diagnose: func(ctx context.Context) string { return kube.Diagnose(ctx, e.k, ns) },
	}
}

func (e *Engine) stepRepoSecret(p *protocol.SetupPlan) step {
	ns := p.ArgoCD.Namespace
	return step{
		id:      "repo-secret",
		title:   "Create repository Secret for Argo CD",
		command: fmt.Sprintf("kubectl apply -n %s -f %s.yaml  # Secret with label argocd.argoproj.io/secret-type=repository", ns, repoSecretName),
		timeout: time.Minute,
		done: func(ctx context.Context) (bool, error) {
			s, err := e.k.Kube.CoreV1().Secrets(ns).Get(ctx, repoSecretName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return err == nil && string(s.Data["url"]) == p.RepoURL, err
		},
		run: func(ctx context.Context) error {
			data := map[string]string{"type": "git", "url": p.RepoURL}
			switch p.Provider {
			case protocol.ProviderLocal:
				creds, err := e.gitea.Credentials(ctx)
				if err != nil {
					return err
				}
				if creds == nil {
					return errors.New("gitea credentials not found")
				}
				data["username"] = gitea.ReaderUser
				data["password"] = creds.ReaderPassword
			case protocol.ProviderGitHub:
				c := p.Credentials
				if c == nil {
					return errors.New("GitHub App credentials are missing; apply setup again with credentials")
				}
				data["githubAppID"] = strconv.FormatInt(c.GitHubAppID, 10)
				data["githubAppInstallationID"] = strconv.FormatInt(c.GitHubAppInstallationID, 10)
				data["githubAppPrivateKey"] = c.GitHubAppPrivateKey
			}
			bin := map[string][]byte{}
			for k, v := range data {
				bin[k] = []byte(v)
			}
			return e.k.UpsertSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      repoSecretName,
					Namespace: ns,
					Labels: map[string]string{
						"argocd.argoproj.io/secret-type": "repository",
						"app.kubernetes.io/managed-by":   "ladder",
					},
				},
				Data: bin,
			})
		},
	}
}

func (e *Engine) stepRootApp(p *protocol.SetupPlan) step {
	ns := p.ArgoCD.Namespace
	const name = "root"
	return step{
		id:      "root-app",
		title:   "Apply root Application",
		command: "kubectl apply -f " + fileRootApp,
		timeout: time.Minute,
		done: func(ctx context.Context) (bool, error) {
			_, err := e.k.Dynamic.Resource(kube.ApplicationGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return err == nil, err
		},
		run: func(ctx context.Context) error {
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(p.Files[fileRootApp]), &obj.Object); err != nil {
				return fmt.Errorf("%s: %w", fileRootApp, err)
			}
			if obj.GetNamespace() == "" {
				obj.SetNamespace(ns)
			}
			_, err := e.k.Apply(ctx, obj)
			return err
		},
	}
}

func (e *Engine) stepSync(p *protocol.SetupPlan) step {
	ns := p.ArgoCD.Namespace
	apps := []string{"root", "argocd", "appsets"}
	check := func(ctx context.Context) (bool, []string, error) {
		var lines []string
		ok := true
		for _, name := range apps {
			u, err := e.k.Dynamic.Resource(kube.ApplicationGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				ok = false
				lines = append(lines, name+": not created yet")
				continue
			}
			if err != nil {
				return false, nil, err
			}
			sync, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
			health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
			line := fmt.Sprintf("%s: %s, %s", name, or(sync, "Unknown"), or(health, "Unknown"))
			if msg, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message"); msg != "" {
				line += ": " + msg
			}
			conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
			for _, c := range conds {
				if m, ok := c.(map[string]any); ok {
					line += fmt.Sprintf("; %v: %v", m["type"], m["message"])
				}
			}
			lines = append(lines, line)
			if sync != "Synced" || health != "Healthy" {
				ok = false
			}
		}
		return ok, lines, nil
	}
	return step{
		id:      "sync",
		title:   "Wait for sync",
		command: fmt.Sprintf("kubectl -n %s get applications %s", ns, strings.Join(apps, " ")),
		timeout: 5 * time.Minute,
		done: func(ctx context.Context) (bool, error) {
			ok, _, err := check(ctx)
			return ok, err
		},
		run: func(ctx context.Context) error {
			return kube.Poll(ctx, 5*time.Second, func() (bool, error) {
				ok, _, err := check(ctx)
				return ok, err
			})
		},
		diagnose: func(ctx context.Context) string {
			_, lines, err := check(ctx)
			if err != nil {
				return err.Error()
			}
			return strings.Join(lines, "\n")
		},
	}
}

func (e *Engine) stepSaveConnection(p *protocol.SetupPlan) step {
	ns := e.cfg.Namespace
	return step{
		id:      "save-connection",
		title:   "Save connection",
		command: fmt.Sprintf("kubectl -n %s create secret generic %s --from-literal=provider=%s --from-literal=repoURL=%s", ns, ConnectionSecret, p.Provider, p.RepoURL),
		timeout: time.Minute,
		done: func(ctx context.Context) (bool, error) {
			_, err := e.k.Kube.CoreV1().Secrets(ns).Get(ctx, ConnectionSecret, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return err == nil, err
		},
		run: func(ctx context.Context) error {
			data := map[string]string{
				"provider":        p.Provider,
				"repoURL":         p.RepoURL,
				"branch":          "main",
				"writeMode":       p.WriteMode,
				"argocdNamespace": p.ArgoCD.Namespace,
				"argocdExposure":  p.ArgoCD.Exposure,
				"argocdHost":      p.ArgoCD.Host,
			}
			if p.Provider == protocol.ProviderLocal {
				data["giteaURL"] = gitea.InClusterURL()
				data["credentialsRef"] = gitea.Namespace + "/" + gitea.AdminSecret
			}
			bin := map[string][]byte{}
			for k, v := range data {
				bin[k] = []byte(v)
			}
			return e.k.UpsertSecret(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: ConnectionSecret, Namespace: ns, Labels: managedLabels()},
				Data:       bin,
			})
		},
	}
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
