// Package appsecrets writes app env secrets directly into the cluster. It
// accepts neither arbitrary manifests nor reads that return secret values.
package appsecrets

import (
	"bytes"
	"context"
	"regexp"
	"strings"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/platform"
	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	appRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,51}[a-z0-9])?$`)
	nsRE      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	versionRE = regexp.MustCompile(`^[a-f0-9]{32}$`)
	keyRE     = regexp.MustCompile(`^[-._A-Za-z0-9]{1,253}$`)
)

func Create(ctx context.Context, k *kube.Clients, req protocol.AppSecretsCreate) error {
	prefix := "ladder-" + req.App + "-env-"
	if !appRE.MatchString(req.App) || !nsRE.MatchString(req.Namespace) ||
		req.Namespace == "ladder" || req.Namespace == "argocd" || strings.HasPrefix(req.Namespace, "kube-") ||
		!strings.HasPrefix(req.Name, prefix) || !versionRE.MatchString(strings.TrimPrefix(req.Name, prefix)) {
		return protocol.Errorf(protocol.CodeBadRequest, "Invalid app secret identity")
	}
	if len(req.Values) == 0 || len(req.Values) > 256 {
		return protocol.Errorf(protocol.CodeBadRequest, "Set between 1 and 256 secret keys")
	}
	data, size := make(map[string][]byte, len(req.Values)), 0
	for key, value := range req.Values {
		if !keyRE.MatchString(key) {
			return protocol.Errorf(protocol.CodeBadRequest, "Invalid secret key")
		}
		size += len(key) + len(value)
		data[key] = []byte(value)
	}
	if size > 512<<10 {
		return protocol.Errorf(protocol.CodeBadRequest, "Secret values exceed 512 KiB")
	}
	// Typed clients avoid kubectl's last-applied annotation and any echo of
	// submitted values. Kubernetes/admission errors may contain the request:
	// return only fixed messages to the cloud and the link logger.
	// The namespace of an app is one Ladder made, labelled so: apps sync
	// only into those (spec §5.8), and no other namespace gets Secrets.
	ns, err := k.Kube.CoreV1().Namespaces().Get(ctx, req.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = k.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: req.Namespace, Labels: map[string]string{platform.AppsLabel: "true"},
		}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return protocol.Errorf(protocol.CodeConflict, "The app namespace was just made; try again")
		}
		if err != nil {
			return protocol.Errorf(protocol.CodeInternal, "Could not create the app namespace")
		}
	} else if err != nil {
		return protocol.Errorf(protocol.CodeInternal, "Could not read the app namespace")
	} else if ns.Labels[platform.AppsLabel] != "true" {
		return protocol.Errorf(protocol.CodeFailedPrecondition, "Namespace %s is not one Ladder made for apps (label %s=true)", req.Namespace, platform.AppsLabel)
	}
	immutable := true
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: req.Name, Namespace: req.Namespace,
		Labels: map[string]string{"ladder.dev/app-secrets": req.App},
	}, Type: corev1.SecretTypeOpaque, Immutable: &immutable, Data: data}
	secrets := k.Kube.CoreV1().Secrets(req.Namespace)
	_, err = secrets.Create(ctx, s, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Retrying a failed Git write is safe, but a version never changes.
		cur, readErr := secrets.Get(ctx, req.Name, metav1.GetOptions{})
		if apierrors.IsForbidden(readErr) {
			// The agent writes Secrets it may not read back. The name is
			// a random version per change: one that exists is this change's,
			// sent again after a lost answer.
			return nil
		}
		if readErr != nil {
			return protocol.Errorf(protocol.CodeInternal, "Could not check the secret version")
		}
		same := cur.Labels["ladder.dev/app-secrets"] == req.App && cur.Immutable != nil && *cur.Immutable && len(cur.Data) == len(data)
		for key, value := range data {
			old, ok := cur.Data[key]
			same = same && ok && bytes.Equal(old, value)
		}
		if !same {
			return protocol.Errorf(protocol.CodeConflict, "This secret version already exists; edit the value and try again")
		}
		return nil
	}
	if err != nil {
		return protocol.Errorf(protocol.CodeInternal, "Could not save the app secrets")
	}
	return nil
}
