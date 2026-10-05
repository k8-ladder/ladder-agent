package setup

import (
	"context"
	"encoding/json"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	StateConfigMap    = "ladder-setup-state"
	CredentialsSecret = "ladder-setup-credentials"
	ConnectionSecret  = "ladder-connection"
)

// store keeps setup progress in ConfigMap ladder-setup-state so that a
// restarted agent resumes. Repository credentials go to a Secret instead.
type store struct {
	k  *kube.Clients
	ns string
}

func (s *store) load(ctx context.Context) (*protocol.SetupPlan, *protocol.SetupStatus, error) {
	cm, err := s.k.Kube.CoreV1().ConfigMaps(s.ns).Get(ctx, StateConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var plan protocol.SetupPlan
	var status protocol.SetupStatus
	if err := json.Unmarshal([]byte(cm.Data["plan.json"]), &plan); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(cm.Data["status.json"]), &status); err != nil {
		return nil, nil, err
	}
	creds, err := s.loadCredentials(ctx)
	if err != nil {
		return nil, nil, err
	}
	plan.Credentials = creds
	return &plan, &status, nil
}

func (s *store) save(ctx context.Context, plan *protocol.SetupPlan, status *protocol.SetupStatus) error {
	stripped := *plan
	stripped.Credentials = nil
	pj, err := json.Marshal(stripped)
	if err != nil {
		return err
	}
	sj, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	data := map[string]string{"plan.json": string(pj), "status.json": string(sj)}

	cms := s.k.Kube.CoreV1().ConfigMaps(s.ns)
	cm, err := cms.Get(ctx, StateConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cms.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: StateConfigMap, Namespace: s.ns, Labels: managedLabels()},
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

func (s *store) saveCredentials(ctx context.Context, c *protocol.RepoCredentials) error {
	if c == nil {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.k.UpsertSecret(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: CredentialsSecret, Namespace: s.ns, Labels: managedLabels()},
		Data:       map[string][]byte{"credentials.json": b},
	})
}

func (s *store) loadCredentials(ctx context.Context) (*protocol.RepoCredentials, error) {
	sec, err := s.k.Kube.CoreV1().Secrets(s.ns).Get(ctx, CredentialsSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c protocol.RepoCredentials
	if err := json.Unmarshal(sec.Data["credentials.json"], &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func managedLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "ladder"}
}
