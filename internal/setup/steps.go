package setup

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ladder/ladder-agent/internal/helmx"
	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	argoIngress = "argocd-server"
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
	return []step{
		e.stepCheckCluster(p),
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
	}
}

// stepCheckCluster stops setup before it changes anything when the cluster
// runs an Argo CD that Ladder did not install. An Argo CD in a namespace
// labelled as Ladder's is from an earlier attempt and passes.
func (e *Engine) stepCheckCluster(p *protocol.SetupPlan) step {
	ns := p.ArgoCD.Namespace
	return step{
		id:      "check-cluster",
		title:   "Check the cluster",
		command: "kubectl get crd applications.argoproj.io",
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
			return nil
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
			if rel != nil && rel.Status == "deployed" {
				return nil
			}
			if rel != nil {
				// An earlier attempt failed or was cut off, such as by the
				// cluster's API dropping the connection, and its release
				// blocks a new install. In Ladder's namespace it is setup's
				// own: remove it and install again.
				ours, err := e.k.NamespaceHasLabels(ctx, ns, managedLabels())
				if err != nil {
					return err
				}
				if !ours {
					return fmt.Errorf("release %s is %s; inspect with `helm -n %s history %s`, then uninstall it and retry", argoRelease, rel.Status, ns, argoRelease)
				}
				e.log.Info("removing the release of an earlier attempt", "release", argoRelease, "status", rel.Status)
				if err := e.helm.Uninstall(ns, argoRelease, 5*time.Minute); err != nil {
					return err
				}
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
	// data is the Secret's content: the repository with its credentials.
	data := func() (map[string][]byte, error) {
		c := p.Credentials
		if c == nil {
			return nil, errors.New("the repository credentials are missing; apply setup again with credentials")
		}
		d := map[string]string{"type": "git", "url": p.RepoURL}
		switch p.Provider {
		case protocol.ProviderLadder:
			d["username"] = c.Username
			d["password"] = c.Password
		case protocol.ProviderGitHub:
			if c.SSHPrivateKey != "" {
				d["sshPrivateKey"] = c.SSHPrivateKey
				break
			}
			d["githubAppID"] = strconv.FormatInt(c.GitHubAppID, 10)
			d["githubAppInstallationID"] = strconv.FormatInt(c.GitHubAppInstallationID, 10)
			d["githubAppPrivateKey"] = c.GitHubAppPrivateKey
		}
		bin := map[string][]byte{}
		for k, v := range d {
			bin[k] = []byte(v)
		}
		return bin, nil
	}
	return step{
		id:      "repo-secret",
		title:   "Create repository Secret for Argo CD",
		command: fmt.Sprintf("kubectl apply -n %s -f %s.yaml  # Secret with label argocd.argoproj.io/secret-type=repository", ns, repoSecretName),
		timeout: time.Minute,
		// Done when the Secret holds this plan's repository and credentials:
		// setup applied again may bring a new token.
		done: func(ctx context.Context) (bool, error) {
			s, err := e.k.Kube.CoreV1().Secrets(ns).Get(ctx, repoSecretName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			want, err := data()
			if err != nil {
				return false, err
			}
			for k, v := range want {
				if string(s.Data[k]) != string(v) {
					return false, nil
				}
			}
			return true, nil
		},
		run: func(ctx context.Context) error {
			bin, err := data()
			if err != nil {
				return err
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
			ready, line, err := appSynced(ctx, e.k, u)
			if err != nil {
				return false, nil, err
			}
			lines = append(lines, name+": "+line)
			ok = ok && ready
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

// appSynced says whether an Application is Synced and Healthy, and describes
// its state. Argo CD reports an Ingress without an address as Progressing, and
// a cluster without an ingress controller, such as DOKS, never gives one: an
// Application that is Progressing while some of its Ingresses have no address
// and its Deployments and StatefulSets have rolled out counts as healthy.
// Argo CD 3 keeps the health of each resource out of the Application
// (status.resourceHealthSource appTree), so these are read from the cluster.
func appSynced(ctx context.Context, k *kube.Clients, u *unstructured.Unstructured) (bool, string, error) {
	sync, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	line := fmt.Sprintf("%s, %s", or(sync, "Unknown"), or(health, "Unknown"))
	if msg, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "message"); msg != "" {
		line += ": " + msg
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok {
			line += fmt.Sprintf("; %v: %v", m["type"], m["message"])
		}
	}
	if sync != "Synced" || health != "Progressing" {
		return sync == "Synced" && health == "Healthy", line, nil
	}
	noAddress, notReady := 0, 0
	res, _, _ := unstructured.NestedSlice(u.Object, "status", "resources")
	for _, r := range res {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		group, _, _ := unstructured.NestedString(m, "group")
		kind, _, _ := unstructured.NestedString(m, "kind")
		ns, _, _ := unstructured.NestedString(m, "namespace")
		name, _, _ := unstructured.NestedString(m, "name")
		var ready bool
		var err error
		switch group + "/" + kind {
		case "networking.k8s.io/Ingress":
			var ing *networkingv1.Ingress
			if ing, err = k.Kube.NetworkingV1().Ingresses(ns).Get(ctx, name, metav1.GetOptions{}); err == nil && len(kube.IngressAddresses(ing)) == 0 {
				noAddress++
				line += fmt.Sprintf("; Ingress %s/%s has no address (no ingress controller in the cluster?)", ns, name)
			}
			continue
		case "apps/Deployment":
			var d *appsv1.Deployment
			if d, err = k.Kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				ready = kube.DeploymentReady(d)
			}
		case "apps/StatefulSet":
			var st *appsv1.StatefulSet
			if st, err = k.Kube.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				ready = kube.StatefulSetReady(st)
			}
		default:
			continue
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return false, "", err
		}
		if !ready {
			notReady++
			line += fmt.Sprintf("; %s %s/%s is not ready", kind, ns, name)
		}
	}
	return noAddress > 0 && notReady == 0, line, nil
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
