package gitea

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/ladder/ladder-agent/internal/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	Namespace   = "ladder-git"
	AdminSecret = "ladder-git-admin"
	AdminUser   = "ladder"
	ReaderUser  = "argocd"
	Owner       = AdminUser
	Deployment  = "gitea"
	Service     = "gitea-http"
	Port        = 3000
)

// InClusterURL is how the agent and Argo CD reach Gitea.
func InClusterURL() string { return fmt.Sprintf("http://%s.%s.svc:%d", Service, Namespace, Port) }

// RepoURL is the clone URL Argo CD reads.
func RepoURL(repo string) string { return fmt.Sprintf("%s/%s/%s.git", InClusterURL(), Owner, repo) }

type Credentials struct {
	AdminPassword  string
	ReaderPassword string
	Token          string
}

type Installer struct {
	k       *kube.Clients
	image   string
	baseURL string
	log     *slog.Logger
}

// NewInstaller: baseURL overrides InClusterURL when the agent runs outside the cluster.
func NewInstaller(k *kube.Clients, image, baseURL string, log *slog.Logger) *Installer {
	if baseURL == "" {
		baseURL = InClusterURL()
	}
	return &Installer{k: k, image: image, baseURL: strings.TrimRight(baseURL, "/"), log: log}
}

// Credentials reads Secret ladder-git-admin; nil if it does not exist yet.
func (i *Installer) Credentials(ctx context.Context) (*Credentials, error) {
	s, err := i.k.Kube.CoreV1().Secrets(Namespace).Get(ctx, AdminSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Credentials{
		AdminPassword:  string(s.Data["admin-password"]),
		ReaderPassword: string(s.Data["argocd-password"]),
		Token:          string(s.Data["token"]),
	}, nil
}

// Client returns an API client authenticated with the admin token.
func (i *Installer) Client(ctx context.Context) (*Client, error) {
	creds, err := i.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	if creds == nil || creds.Token == "" {
		return nil, fmt.Errorf("secret %s/%s has no API token yet", Namespace, AdminSecret)
	}
	c := NewClient(i.baseURL)
	c.Token = creds.Token
	return c, nil
}

// RepoReady is the "done when" check of the Gitea step.
func (i *Installer) RepoReady(ctx context.Context, repo string) (bool, error) {
	creds, err := i.Credentials(ctx)
	if err != nil || creds == nil || creds.Token == "" {
		return false, err
	}
	c, err := i.Client(ctx)
	if err != nil {
		return false, err
	}
	if !c.Healthy(ctx) {
		return false, nil
	}
	return c.RepoExists(ctx, Owner, repo)
}

// Install brings up Gitea and creates the users and the repository. Every
// part is idempotent, so a failed or interrupted install can simply rerun.
func (i *Installer) Install(ctx context.Context, repo string) error {
	if err := i.k.EnsureNamespace(ctx, Namespace); err != nil {
		return err
	}
	creds, err := i.ensureSecret(ctx)
	if err != nil {
		return fmt.Errorf("admin secret: %w", err)
	}
	if err := i.applyManifests(ctx); err != nil {
		return err
	}
	i.log.Info("waiting for gitea")
	if err := kube.WaitDeploymentsReady(ctx, i.k, Namespace, Deployment); err != nil {
		return fmt.Errorf("gitea rollout: %w", err)
	}

	c := NewClient(i.baseURL)
	if err := kube.Poll(ctx, 2*time.Second, func() (bool, error) { return c.Healthy(ctx), nil }); err != nil {
		return fmt.Errorf("gitea API at %s not reachable: %w", i.baseURL, err)
	}
	if creds.Token == "" {
		token, err := c.CreateToken(ctx, AdminUser, creds.AdminPassword, "ladder-agent-"+randomString(4))
		if err != nil {
			return fmt.Errorf("create API token: %w", err)
		}
		if err := i.saveToken(ctx, token); err != nil {
			return fmt.Errorf("save API token: %w", err)
		}
		creds.Token = token
	}
	c.Token = creds.Token

	if err := c.EnsureUser(ctx, ReaderUser, creds.ReaderPassword, ReaderUser+"@ladder.local"); err != nil {
		return fmt.Errorf("create user %s: %w", ReaderUser, err)
	}
	if err := c.EnsureRepo(ctx, repo); err != nil {
		return fmt.Errorf("create repository: %w", err)
	}
	if err := c.AddCollaborator(ctx, Owner, repo, ReaderUser, "read"); err != nil {
		return fmt.Errorf("grant %s read access: %w", ReaderUser, err)
	}
	return nil
}

// ensureSecret creates the admin Secret once. Passwords are never regenerated:
// the admin user in Gitea's database was created with the first one.
func (i *Installer) ensureSecret(ctx context.Context) (*Credentials, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: AdminSecret, Namespace: Namespace, Labels: labels()},
		StringData: map[string]string{
			"admin-password":  randomString(24),
			"argocd-password": randomString(24),
		},
	}
	_, err := i.k.Kube.CoreV1().Secrets(Namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	return i.Credentials(ctx)
}

func (i *Installer) saveToken(ctx context.Context, token string) error {
	s, err := i.k.Kube.CoreV1().Secrets(Namespace).Get(ctx, AdminSecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data["token"] = []byte(token)
	_, err = i.k.Kube.CoreV1().Secrets(Namespace).Update(ctx, s, metav1.UpdateOptions{})
	return err
}

func (i *Installer) applyManifests(ctx context.Context) error {
	var buf bytes.Buffer
	if err := manifests.Execute(&buf, map[string]any{"Image": i.image, "Namespace": Namespace, "Secret": AdminSecret, "Service": Service, "Port": Port, "RootURL": InClusterURL() + "/"}); err != nil {
		return err
	}
	for _, doc := range strings.Split(buf.String(), "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		if _, err := i.k.ApplyYAML(ctx, []byte(doc)); err != nil {
			return fmt.Errorf("apply gitea manifests: %w", err)
		}
	}
	return nil
}

func labels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": "gitea", "app.kubernetes.io/managed-by": "ladder"}
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

// The data volume holds the SQLite database, the repositories and app.ini, so
// a restart keeps Gitea's generated secrets.
var manifests = template.Must(template.New("gitea").Parse(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: gitea-data
  namespace: {{.Namespace}}
  labels: {app.kubernetes.io/name: gitea, app.kubernetes.io/managed-by: ladder}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  name: {{.Service}}
  namespace: {{.Namespace}}
  labels: {app.kubernetes.io/name: gitea, app.kubernetes.io/managed-by: ladder}
spec:
  selector: {app.kubernetes.io/name: gitea}
  ports:
    - name: http
      port: {{.Port}}
      targetPort: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gitea
  namespace: {{.Namespace}}
  labels: {app.kubernetes.io/name: gitea, app.kubernetes.io/managed-by: ladder}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels: {app.kubernetes.io/name: gitea}
  template:
    metadata:
      labels: {app.kubernetes.io/name: gitea}
    spec:
      securityContext: {runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000, runAsNonRoot: true}
      initContainers:
        - name: init
          image: {{.Image}}
          command: [/usr/local/bin/docker-entrypoint.sh]
          args:
            - sh
            - -c
            - |
              set -e
              gitea -c "$GITEA_APP_INI" migrate
              if gitea -c "$GITEA_APP_INI" admin user list --admin | awk 'NR>1 {print $2}' | grep -qx ladder; then
                echo "admin user exists"
              else
                gitea -c "$GITEA_APP_INI" admin user create --admin --username ladder \
                  --password "$ADMIN_PASSWORD" --email ladder@ladder.local --must-change-password=false
              fi
          env: &env
            - {name: GITEA_APP_INI, value: /var/lib/gitea/custom/conf/app.ini}
            - {name: GITEA__security__INSTALL_LOCK, value: "true"}
            - {name: GITEA__server__ROOT_URL, value: "{{.RootURL}}"}
            - {name: GITEA__server__DISABLE_SSH, value: "true"}
            - {name: GITEA__server__START_SSH_SERVER, value: "false"}
            - {name: GITEA__service__DISABLE_REGISTRATION, value: "true"}
            - {name: GITEA__database__DB_TYPE, value: sqlite3}
            - {name: GITEA__repository__DEFAULT_BRANCH, value: main}
            - name: ADMIN_PASSWORD
              valueFrom: {secretKeyRef: {name: {{.Secret}}, key: admin-password}}
          volumeMounts:
            - {name: data, mountPath: /var/lib/gitea}
      containers:
        - name: gitea
          image: {{.Image}}
          env: *env
          ports:
            - {name: http, containerPort: 3000}
          readinessProbe:
            httpGet: {path: /api/healthz, port: http}
            periodSeconds: 5
          livenessProbe:
            httpGet: {path: /api/healthz, port: http}
            initialDelaySeconds: 30
            periodSeconds: 20
          resources:
            requests: {cpu: 50m, memory: 128Mi}
            limits: {memory: 256Mi}
          volumeMounts:
            - {name: data, mountPath: /var/lib/gitea}
      volumes:
        - name: data
          persistentVolumeClaim: {claimName: gitea-data}
`))
