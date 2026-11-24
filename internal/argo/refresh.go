package argo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/k8-ladder/ladder-agent/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Refresher makes Argo CD read git now instead of at its next poll (about
// three minutes), on a push to main (spec §5.5).
type Refresher struct {
	K *kube.Clients
	// Client posts to Argo CD's webhook.
	Client *http.Client
	// Webhook is Argo CD's webhook endpoint; empty means its Service in the
	// namespace.
	Webhook string
}

// Refresh refreshes the Applications and ApplicationSets in namespace for a
// push of main of repoURL.
//
// Applications: the agent posts the push to Argo CD's webhook, as a git
// server would, and Argo CD refreshes the Applications of that repository; so
// the agent needs no right on them. If the webhook fails, it annotates them,
// which only an agent with setup access may.
//
// ApplicationSets: Argo CD's webhook for them does not start reliably, so
// the agent annotates them, which generates their Applications again, and a
// new app gets one. Without setup access, the policy ladder-agent-refresh
// lets it change nothing else of theirs. Argo CD removes the annotations once
// it has acted on them. Before setup has installed Argo CD there is nothing to
// refresh.
func (r *Refresher) Refresh(ctx context.Context, namespace, repoURL string) error {
	var errs []error
	err := r.postPush(ctx, namespace, repoURL)
	if err != nil {
		err = fmt.Errorf("Argo CD's webhook: %w", err)
		if aerr := annotate(ctx, r.K, kube.ApplicationGVR, namespace, "argocd.argoproj.io/refresh", "normal"); aerr != nil {
			errs = append(errs, err, aerr)
		}
	}
	if err := annotate(ctx, r.K, kube.ApplicationSetGVR, namespace, "argocd.argoproj.io/application-set-refresh", "true"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// postPush posts a push of main to Argo CD's webhook in the format of Gogs,
// which Gitea's matches, naming the repository by its web address.
func (r *Refresher) postPush(ctx context.Context, namespace, repoURL string) error {
	if repoURL == "" {
		return errors.New("no repository: setup has not run")
	}
	body, _ := json.Marshal(map[string]any{
		"ref":        "refs/heads/main",
		"repository": map[string]any{"html_url": WebURL(repoURL), "clone_url": repoURL},
	})
	url := r.Webhook
	if url == "" {
		url = "http://argocd-server." + namespace + ".svc/argocd/api/webhook"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gogs-Event", "push")
	resp, err := r.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// WebURL is the repository's address as a git server's push names it, which
// Argo CD matches with an Application's repoURL: https://github.com/acme/prod
// for ssh://git@github.com/acme/prod.git.
func WebURL(repoURL string) string {
	u := strings.TrimSuffix(repoURL, ".git")
	if rest, ok := strings.CutPrefix(u, "ssh://git@"); ok {
		return "https://" + rest
	}
	if rest, ok := strings.CutPrefix(u, "git@"); ok {
		host, path, _ := strings.Cut(rest, ":")
		return "https://" + host + "/" + path
	}
	return u
}

func annotate(ctx context.Context, k *kube.Clients, gvr schema.GroupVersionResource, namespace, annotation, value string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{annotation: value}}})
	ri := k.Dynamic.Resource(gvr).Namespace(namespace)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil // no CRD yet
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, o := range list.Items {
		_, err := ri.Patch(ctx, o.GetName(), types.MergePatchType, patch, metav1.PatchOptions{FieldManager: kube.FieldManager})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
