// Command agent is ladder-agent: the in-cluster part of Ladder. It dials
// ladder-be, reports cluster facts and Argo CD status, runs first-run setup,
// and forwards the cloud's calls to the in-cluster Gitea.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ladder/ladder-agent/internal/argo"
	"github.com/ladder/ladder-agent/internal/cluster"
	"github.com/ladder/ladder-agent/internal/config"
	"github.com/ladder/ladder-agent/internal/gitea"
	"github.com/ladder/ladder-agent/internal/helmx"
	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/link"
	"github.com/ladder/ladder-agent/internal/protocol"
	"github.com/ladder/ladder-agent/internal/setup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	k, err := kube.New()
	if err != nil {
		return err
	}
	hello, err := helloInfo(ctx, k)
	if err != nil {
		return err
	}
	log.Info("starting", "version", Version, "context", k.Context, "kubernetes", hello.KubernetesVersion)

	inspector := cluster.NewInspector(k)
	giteaInstaller := gitea.NewInstaller(k, cfg.GiteaImage, cfg.GiteaURL, log.With("component", "gitea"))
	engine := setup.NewEngine(ctx, k, helmx.New(log.With("component", "helm")),
		giteaInstaller, inspector, cfg, log.With("component", "setup"))
	be := link.New(cfg.BackendURL, cfg.Token, log.With("component", "link"))

	engine.OnChange = func(s protocol.SetupStatus) { be.Emit(protocol.EventSetupStatus, s) }
	if err := engine.Load(ctx); err != nil {
		return err
	}

	argoNS := cfg.ArgoCDNamespace
	if ns := engine.ArgoCDNamespace(); ns != "" {
		argoNS = ns
	}
	watcher := argo.NewWatcher(k, argoNS, log.With("component", "argo"))
	watcher.OnChange = func(c protocol.AppsChanged) { be.Emit(protocol.EventAppsChanged, c) }
	watcher.OnSnapshot = func(s protocol.AppsSnapshot) { be.Emit(protocol.EventAppsSnapshot, s) }

	be.OnConnect(func() {
		be.Emit(protocol.EventHello, hello)
		be.Emit(protocol.EventSetupStatus, engine.Status())
		be.Emit(protocol.EventAppsSnapshot, watcher.Snapshot())
	})
	be.Handle(protocol.MethodClusterInfo, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return inspector.Info(ctx)
	})
	be.Handle(protocol.MethodAppsStatus, func(context.Context, json.RawMessage) (any, error) {
		return watcher.Snapshot(), nil
	})
	be.Handle(protocol.MethodSetupStatus, func(context.Context, json.RawMessage) (any, error) {
		return engine.Status(), nil
	})
	be.Handle(protocol.MethodSetupPlan, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p protocol.SetupPlan
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return engine.Plan(ctx, p)
	})
	be.Handle(protocol.MethodSetupApply, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p protocol.SetupPlan
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return engine.Apply(ctx, p)
	})
	be.Handle(protocol.MethodSetupRetry, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return engine.Retry(ctx)
	})
	be.Handle(protocol.MethodGitHTTP, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req protocol.GitHTTPRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return giteaInstaller.Proxy(ctx, engine.Repo(), req)
	})
	be.Handle(protocol.MethodArgoCDCredentials, func(ctx context.Context, _ json.RawMessage) (any, error) {
		ns := engine.ArgoCDNamespace()
		if ns == "" {
			ns = cfg.ArgoCDNamespace
		}
		return argo.InitialCredentials(ctx, k, ns)
	})

	go watcher.Run(ctx)
	go serveHealth(ctx, cfg.HealthAddr, log)
	be.Run(ctx)
	log.Info("shutting down")
	return nil
}

func helloInfo(ctx context.Context, k *kube.Clients) (protocol.Hello, error) {
	v, err := k.Kube.Discovery().ServerVersion()
	if err != nil {
		return protocol.Hello{}, err
	}
	h := protocol.Hello{AgentVersion: Version, Context: k.Context, KubernetesVersion: v.GitVersion}
	// The kube-system namespace UID is a stable cluster identity.
	if ns, err := k.Kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{}); err == nil {
		h.ClusterID = string(ns.UID)
	}
	return h, nil
}

func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return protocol.Errorf(protocol.CodeBadRequest, "payload is required")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return protocol.Errorf(protocol.CodeBadRequest, "invalid payload: %v", err)
	}
	return nil
}

func serveHealth(ctx context.Context, addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("health server", "err", err)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
