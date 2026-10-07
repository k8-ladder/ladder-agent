// Package config reads agent settings from environment variables.
package config

import (
	"errors"
	"os"
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
		HTTPAddr:           env("HTTP_ADDR", ":8081"),
		LogLevel:           env("LOG_LEVEL", "info"),
	}
	if c.BackendURL == "" {
		return c, errors.New("LADDER_BE_URL is required")
	}
	if c.Token == "" {
		return c, errors.New("LADDER_AGENT_TOKEN is required")
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
