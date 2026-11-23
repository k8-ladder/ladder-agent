// Package config reads agent settings from environment variables.
package config

import (
	"errors"
	"os"
	"strings"
)

type Config struct {
	// BackendURL is the WebSocket endpoint of ladder-be, for example
	// ws://ladder-be.ladder.svc/agent/connect.
	BackendURL string
	// Token authenticates the agent to the backend (Authorization: Bearer).
	Token string

	// Namespace holds the agent's own state: ConfigMap ladder-setup-state and
	// Secrets ladder-*.
	Namespace string
	// ArgoCDNamespace is where Ladder apps' Applications live, unless setup
	// state says otherwise.
	ArgoCDNamespace string

	ArgoCDVersion      string
	ArgoCDChartVersion string
	ArgoCDChartRepo    string

	// What the people who run the cluster allow Ladder, in the agent's
	// Deployment; an upgrade keeps them, and the cloud cannot change them.
	//
	// RemoteUpgrade: agent.upgrade from the cloud (LADDER_REMOTE_UPGRADE=off
	// turns it off).
	RemoteUpgrade bool
	// Signature is LADDER_AGENT_SIGNATURE: "keyless" runs only images Ladder's
	// release workflow signed, "off" any of the agent's repository, and
	// empty, the default, "off" for an agent whose image is in no registry
	// and "keyless" otherwise. Signer and SignerIssuer override the
	// workflow's identity (LADDER_AGENT_SIGNER, LADDER_AGENT_SIGNER_ISSUER).
	Signature, Signer, SignerIssuer string
	// KubeRead: kube.read for the assistant (LADDER_KUBE_READ=off).
	KubeRead bool
	// AppLogs: pod logs on app pages and in kube.read (LADDER_APP_LOGS=off).
	AppLogs bool
	// ReadNamespaces limits kube.read to these namespaces, globs such as
	// shop-*, when set (LADDER_READ_NAMESPACES, comma-separated).
	ReadNamespaces []string

	// HTTPAddr serves /healthz and /readyz.
	HTTPAddr string
	LogLevel string
}

func Load() (Config, error) {
	c := Config{
		BackendURL:         os.Getenv("LADDER_BE_URL"),
		Token:              os.Getenv("LADDER_AGENT_TOKEN"),
		Namespace:          env("POD_NAMESPACE", "ladder"),
		ArgoCDNamespace:    env("ARGOCD_NAMESPACE", "argocd"),
		ArgoCDVersion:      env("ARGOCD_DEFAULT_VERSION", "3.5.3"),
		ArgoCDChartVersion: env("ARGOCD_CHART_VERSION", "10.9.6"),
		ArgoCDChartRepo:    env("ARGOCD_CHART_REPO", "https://argoproj.github.io/argo-helm"),
		RemoteUpgrade:      os.Getenv("LADDER_REMOTE_UPGRADE") != "off",
		Signature:          os.Getenv("LADDER_AGENT_SIGNATURE"),
		Signer:             os.Getenv("LADDER_AGENT_SIGNER"),
		SignerIssuer:       os.Getenv("LADDER_AGENT_SIGNER_ISSUER"),
		KubeRead:           os.Getenv("LADDER_KUBE_READ") != "off",
		AppLogs:            os.Getenv("LADDER_APP_LOGS") != "off",
		ReadNamespaces:     list(os.Getenv("LADDER_READ_NAMESPACES")),
		HTTPAddr:           env("HTTP_ADDR", ":8081"),
		LogLevel:           env("LOG_LEVEL", "info"),
	}
	if c.BackendURL == "" {
		return c, errors.New("LADDER_BE_URL is required")
	}
	if c.Token == "" {
		return c, errors.New("LADDER_AGENT_TOKEN is required")
	}
	if c.Signature != "" && c.Signature != "keyless" && c.Signature != "off" {
		return c, errors.New(`LADDER_AGENT_SIGNATURE must be "keyless" or "off"`)
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func list(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
