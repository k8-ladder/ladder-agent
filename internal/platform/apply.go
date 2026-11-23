package platform

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ladder/ladder-agent/internal/kube"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const (
	// ViewerSecret, in the agent's namespace, holds the read-only account's
	// password, which argocd.credentials hands out.
	ViewerSecret = "ladder-argocd-viewer"
	// ViewerUser is the read-only account.
	ViewerUser = "ladder"
	// StateConfigMap, in the agent's namespace, keeps the hash of the
	// platform the agent last applied.
	StateConfigMap = "ladder-platform-state"

	trackingAnnotation = "argocd.argoproj.io/tracking-id"
)

// legacy are the Applications of the layout before the platform came from
// the agent: root synced bootstrap/ from git, and appsets synced argocd/.
var legacy = []string{"root", "appsets"}

// EnsureViewer returns the read-only account's password, made the first time.
func EnsureViewer(ctx context.Context, k *kube.Clients, namespace string, now time.Time) (Viewer, error) {
	secrets := k.Kube.CoreV1().Secrets(namespace)
	s, err := secrets.Get(ctx, ViewerSecret, metav1.GetOptions{})
	if err == nil {
		return Viewer{Password: string(s.Data["password"]), PasswordHash: string(s.Data["passwordHash"]), PasswordMtime: string(s.Data["passwordMtime"])}, nil
	}
	if !apierrors.IsNotFound(err) {
		return Viewer{}, err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return Viewer{}, err
	}
	v := Viewer{Password: base64.RawURLEncoding.EncodeToString(b), PasswordMtime: now.UTC().Format(time.RFC3339)}
	hash, err := bcrypt.GenerateFromPassword([]byte(v.Password), bcrypt.DefaultCost)
	if err != nil {
		return Viewer{}, err
	}
	v.PasswordHash = string(hash)
	_, err = secrets.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ViewerSecret, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "ladder"}},
		Data: map[string][]byte{
			"username": []byte(ViewerUser), "password": []byte(v.Password),
			"passwordHash": []byte(v.PasswordHash), "passwordMtime": []byte(v.PasswordMtime),
		},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return EnsureViewer(ctx, k, namespace, now)
	}
	return v, err
}

// Overrides reads the Helm values people set in ConfigMap ladder-platform.
// None without it.
func Overrides(ctx context.Context, k *kube.Clients, namespace string) (map[string]string, error) {
	cm, err := k.Kube.CoreV1().ConfigMaps(namespace).Get(ctx, OverridesConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cm.Data, nil
}

// AppliedHash is the hash of the platform the agent last applied, or empty.
func AppliedHash(ctx context.Context, k *kube.Clients, namespace string) (string, error) {
	cm, err := k.Kube.CoreV1().ConfigMaps(namespace).Get(ctx, StateConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return cm.Data["hash"], nil
}

func saveHash(ctx context.Context, k *kube.Clients, namespace, hash string, now time.Time) error {
	cms := k.Kube.CoreV1().ConfigMaps(namespace)
	data := map[string]string{"hash": hash, "appliedAt": now.UTC().Format(time.RFC3339)}
	cm, err := cms.Get(ctx, StateConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: StateConfigMap, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "ladder"}},
			Data:       data,
		}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	cm.Data = data
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// Apply applies the platform: it moves a cluster set up from git to the
// agent's platform, labels the namespaces of existing apps, applies the
// objects server-side, makes Argo CD's Ingress if it has none, and records
// the hash.
func Apply(ctx context.Context, k *kube.Clients, p Params, pl *Platform, log *slog.Logger, now time.Time) error {
	ns := p.ArgoCDNamespace
	apps := k.Dynamic.Resource(kube.ApplicationGVR).Namespace(ns)
	var names []string
	for _, c := range Components(p) {
		names = append(names, c.Name)
	}
	if err := migrate(ctx, k, apps, ns, names, log); err != nil {
		return fmt.Errorf("move the platform out of git: %w", err)
	}
	if err := labelAppNamespaces(ctx, k, apps, p, log); err != nil {
		return fmt.Errorf("label the namespaces of apps: %w", err)
	}
	for _, obj := range pl.Objects {
		applied, err := k.Apply(ctx, obj.DeepCopy())
		if err != nil {
			return fmt.Errorf("apply %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		if err := untrack(ctx, k, applied); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := resync(ctx, apps, name, log); err != nil {
			return fmt.Errorf("sync Application %s: %w", name, err)
		}
	}
	if pl.Ingress != nil {
		if err := ensureIngress(ctx, k, pl.Ingress); err != nil {
			return fmt.Errorf("Argo CD's Ingress: %w", err)
		}
	}
	return saveHash(ctx, k, p.AgentNamespace, pl.Hash, now)
}

// migrate removes the Applications of a cluster set up from git: root and
// appsets, and the platform's own when they still take values from git.
// Without finalizers, deleting them leaves what they deployed running, and
// the agent's Applications of the same names take it over.
func migrate(ctx context.Context, k *kube.Clients, apps dynamic.ResourceInterface, ns string, platform []string, log *slog.Logger) error {
	for _, name := range legacy {
		if err := orphan(ctx, apps, name, log); err != nil {
			return err
		}
	}
	for _, name := range platform {
		u, err := apps.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		project, _, _ := unstructured.NestedString(u.Object, "spec", "project")
		sources, _, _ := unstructured.NestedSlice(u.Object, "spec", "sources")
		if project == Project && len(sources) == 0 {
			continue
		}
		if err := orphan(ctx, apps, name, log); err != nil {
			return err
		}
	}
	// Argo CD's Ingress was an object of the Application argocd.
	ing, err := k.Dynamic.Resource(ingressGVR).Namespace(ns).Get(ctx, Ingress, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return untrack(ctx, k, ing)
}

// resync starts a sync of a platform Application whose last one failed:
// Argo CD does not sync again by itself a revision it failed to sync, though
// the platform applied now may fix what made it fail, such as a project.
func resync(ctx context.Context, apps dynamic.ResourceInterface, name string, log *slog.Logger) error {
	u, err := apps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, running := u.Object["operation"]; running {
		return nil
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
	if phase != "Failed" && phase != "Error" {
		return nil
	}
	op := map[string]any{"initiatedBy": map[string]any{"username": "ladder-agent"}, "sync": map[string]any{}}
	if retry, ok, _ := unstructured.NestedMap(u.Object, "spec", "syncPolicy", "retry"); ok {
		op["retry"] = retry
	}
	patch, _ := json.Marshal(map[string]any{"operation": op})
	log.Info("syncing an Application of the platform again", "application", name, "last", phase)
	_, err = apps.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: kube.FieldManager})
	return err
}

// orphan deletes an Application without what it deployed.
func orphan(ctx context.Context, apps dynamic.ResourceInterface, name string, log *slog.Logger) error {
	if _, err := apps.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	log.Info("removing an Application of the platform from git; what it deployed stays", "application", name)
	if _, err := apps.Patch(ctx, name, types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := apps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return kube.Poll(ctx, time.Second, func() (bool, error) {
		_, err := apps.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// labelAppNamespaces labels the namespaces of the apps the cluster runs, so
// that Argo CD may go on syncing them there (policy ladder-apps-namespaces).
func labelAppNamespaces(ctx context.Context, k *kube.Clients, apps dynamic.ResourceInterface, p Params, log *slog.Logger) error {
	list, err := apps.List(ctx, metav1.ListOptions{LabelSelector: "ladder.dev/managed=true"})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, u := range list.Items {
		ns, _, _ := unstructured.NestedString(u.Object, "spec", "destination", "namespace")
		if ns == "" || done[ns] || !AppNamespace(ns, p) {
			continue
		}
		done[ns] = true
		cur, err := k.Kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if cur.Labels[AppsLabel] == "true" {
			continue
		}
		log.Info("labelling the namespace of an app", "namespace", ns, "label", AppsLabel)
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]string{AppsLabel: "true"}}})
		if _, err := k.Kube.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: kube.FieldManager}); err != nil {
			return err
		}
	}
	return nil
}

// AppNamespace reports whether apps may go into namespace ns, as the
// project apps says.
func AppNamespace(ns string, p Params) bool {
	switch {
	case ns == p.ArgoCDNamespace, ns == p.AgentNamespace, ns == "traefik", ns == "cert-manager", strings.HasPrefix(ns, "kube-"):
		return false
	}
	return true
}

// untrack removes Argo CD's tracking annotation from an object an
// Application of the layout from git applied, so that Argo CD no longer
// counts it as that Application's.
func untrack(ctx context.Context, k *kube.Clients, u *unstructured.Unstructured) error {
	if u == nil {
		return nil
	}
	if _, ok := u.GetAnnotations()[trackingAnnotation]; !ok {
		return nil
	}
	gvk := u.GroupVersionKind()
	m, err := k.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}
	var ri dynamic.ResourceInterface = k.Dynamic.Resource(m.Resource)
	if u.GetNamespace() != "" {
		ri = k.Dynamic.Resource(m.Resource).Namespace(u.GetNamespace())
	}
	patch := []byte(`{"metadata":{"annotations":{"` + trackingAnnotation + `":null}}}`)
	_, err = ri.Patch(ctx, u.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ensureIngress makes Argo CD's Ingress when there is none. An existing one
// stays as people or the agent left it.
func ensureIngress(ctx context.Context, k *kube.Clients, ing *unstructured.Unstructured) error {
	_, err := k.Kube.NetworkingV1().Ingresses(ing.GetNamespace()).Get(ctx, ing.GetName(), metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = k.Apply(ctx, ing.DeepCopy())
	return err
}

// ErrNoViewer means the read-only account is not there yet: setup has not
// reached the platform, or the agent has not applied it since an upgrade.
var ErrNoViewer = errors.New("Argo CD's read-only account is not set up yet")

// ViewerCredentials reads the read-only account's password.
func ViewerCredentials(ctx context.Context, k *kube.Clients, namespace string) (user, password string, err error) {
	s, err := k.Kube.CoreV1().Secrets(namespace).Get(ctx, ViewerSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", "", ErrNoViewer
	}
	if err != nil {
		return "", "", err
	}
	return ViewerUser, string(s.Data["password"]), nil
}
