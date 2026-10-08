// Package helmx wraps the Helm SDK for the few operations setup needs.
package helmx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	release "helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage/driver"
)

type Helm struct {
	log *slog.Logger
}

func New(log *slog.Logger) *Helm { return &Helm{log: log} }

type Release struct {
	Name      string
	Namespace string
	Status    string
	Revision  int
}

type InstallOptions struct {
	ReleaseName string
	Namespace   string
	RepoURL     string
	Chart       string
	Version     string
	Values      map[string]any
	Timeout     time.Duration
}

func (h *Helm) config(namespace string) (*action.Configuration, *cli.EnvSettings, error) {
	settings := cli.New()
	settings.SetNamespace(namespace)
	cfg := action.NewConfiguration()
	cfg.SetLogger(h.log.Handler())
	if err := cfg.Init(settings.RESTClientGetter(), namespace, "secret"); err != nil {
		return nil, nil, err
	}
	return cfg, settings, nil
}

// Status returns the latest release, or nil if there is none.
func (h *Helm) Status(namespace, name string) (*Release, error) {
	cfg, _, err := h.config(namespace)
	if err != nil {
		return nil, err
	}
	rel, err := action.NewStatus(cfg).Run(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	acc, err := release.NewAccessor(rel)
	if err != nil {
		return nil, err
	}
	return &Release{Name: acc.Name(), Namespace: acc.Namespace(), Status: acc.Status(), Revision: acc.Version()}, nil
}

// Install downloads the chart from a classic repository and installs it.
// It waits for hooks only; resource readiness is a separate setup step.
func (h *Helm) Install(ctx context.Context, o InstallOptions) error {
	cfg, settings, err := h.config(o.Namespace)
	if err != nil {
		return err
	}
	install := action.NewInstall(cfg)
	install.ReleaseName = o.ReleaseName
	install.Namespace = o.Namespace
	install.CreateNamespace = true
	install.RepoURL = o.RepoURL
	install.Version = o.Version
	install.WaitStrategy = kube.HookOnlyStrategy
	install.Timeout = o.Timeout

	path, err := install.LocateChart(o.Chart, settings)
	if err != nil {
		return fmt.Errorf("download chart %s %s: %w", o.Chart, o.Version, err)
	}
	ch, err := loader.Load(path)
	if err != nil {
		return fmt.Errorf("load chart: %w", err)
	}
	if _, err := install.RunWithContext(ctx, ch, o.Values); err != nil {
		return fmt.Errorf("helm install %s: %w", o.ReleaseName, err)
	}
	return nil
}

// Uninstall removes a release in any state, with the resources it owns, and
// its history. Resources the chart keeps, such as CRDs, stay.
func (h *Helm) Uninstall(namespace, name string, timeout time.Duration) error {
	cfg, _, err := h.config(namespace)
	if err != nil {
		return err
	}
	uninstall := action.NewUninstall(cfg)
	uninstall.IgnoreNotFound = true
	uninstall.WaitStrategy = kube.HookOnlyStrategy
	uninstall.Timeout = timeout
	if _, err := uninstall.Run(name); err != nil {
		return fmt.Errorf("helm uninstall %s: %w", name, err)
	}
	return nil
}
