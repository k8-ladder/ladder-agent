// Command agent is ladder-agent: the in-cluster part of Ladder. It dials
// ladder-be, reports cluster facts and Argo CD status, runs first-run setup,
// has Argo CD read git as soon as the cloud reports a push, shows the
// assistant cluster objects, events and logs, and replaces itself with a new
// version.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/k8-ladder/ladder-agent/internal/appsecrets"
	"github.com/k8-ladder/ladder-agent/internal/argo"
	"github.com/k8-ladder/ladder-agent/internal/audit"
	"github.com/k8-ladder/ladder-agent/internal/cluster"
	"github.com/k8-ladder/ladder-agent/internal/config"
	"github.com/k8-ladder/ladder-agent/internal/helmx"
	"github.com/k8-ladder/ladder-agent/internal/imagesig"
	"github.com/k8-ladder/ladder-agent/internal/inspect"
	"github.com/k8-ladder/ladder-agent/internal/kube"
	"github.com/k8-ladder/ladder-agent/internal/link"
	"github.com/k8-ladder/ladder-agent/internal/platform"
	"github.com/k8-ladder/ladder-agent/internal/setup"
	"github.com/k8-ladder/ladder-agent/internal/upgrade"
	"github.com/k8-ladder/ladder-agent/protocol"
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
	engine := setup.NewEngine(ctx, k, helmx.New(log.With("component", "helm")),
		inspector, cfg, log.With("component", "setup"))
	be := link.New(cfg.BackendURL, cfg.Token, log.With("component", "link"))

	engine.OnChange = func(s protocol.SetupStatus) { be.Emit(protocol.EventSetupStatus, s) }
	if err := engine.Load(ctx); err != nil {
		return err
	}
	// The Deployment rolls with the old agent running until this one is
	// ready: resume an interrupted setup once that agent has stopped.
	go func() {
		if err := upgrade.WaitAlone(ctx, k, cfg.Namespace, os.Getenv("POD_NAME")); err == nil {
			engine.Resume()
			engine.StartPlatform()
		}
	}()

	argoNS := cfg.ArgoCDNamespace
	if ns := engine.ArgoCDNamespace(); ns != "" {
		argoNS = ns
	}
	watcher := argo.NewWatcher(k, argoNS, log.With("component", "argo"))
	watcher.OnChange = func(c protocol.AppsChanged) { be.Emit(protocol.EventAppsChanged, c) }
	watcher.OnSnapshot = func(s protocol.AppsSnapshot) { be.Emit(protocol.EventAppsSnapshot, s) }

	_, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("HTTP_ADDR: %w", err)
	}
	httpPort, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("HTTP_ADDR: %w", err)
	}
	upgrader := upgrade.New(k, cfg.Namespace, Version, httpPort, log.With("component", "upgrade"))
	upgrader.OnStatus = func(s protocol.AgentUpgrade) { be.Emit(protocol.EventAgentUpgrade, s) }
	upgrader.Allowed = cfg.RemoteUpgrade
	upgrader.Pin = func(ctx context.Context, image string, noRegistry bool) (string, error) {
		v := &imagesig.Verifier{Policy: signaturePolicy(cfg, noRegistry), CachePath: "/tmp/sigstore"}
		return v.Pin(ctx, image)
	}
	// Two agents run while one replaces the other: never during setup.
	upgrader.Busy = func() string {
		if engine.Status().State == protocol.SetupRunning {
			return "setup is running"
		}
		return ""
	}

	// What the cloud asks that changes the cluster or reads more than the
	// apps' status leaves a trace in the cluster (spec §12).
	trail := &audit.Log{Kube: k.Kube, Namespace: cfg.Namespace, Pod: os.Getenv("POD_NAME"), Log: log.With("component", "audit")}
	handle := func(method string, describe audit.Describe, h link.Handler) {
		be.Handle(method, trail.Wrap(method, describe, h))
	}

	be.OnConnect(func() {
		be.Emit(protocol.EventHello, hello)
		be.Emit(protocol.EventSetupStatus, engine.Status())
		be.Emit(protocol.EventAppsSnapshot, watcher.Snapshot())
		if s := upgrader.Status(); s != nil {
			be.Emit(protocol.EventAgentUpgrade, s)
		}
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
	handle(protocol.MethodSetupApply, audit.Field("provider", "repoURL", "traefik"), func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p protocol.SetupPlan
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return engine.Apply(ctx, p)
	})
	handle(protocol.MethodSetupRetry, nil, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return engine.Retry(ctx)
	})
	refresher := &argo.Refresher{K: k, Client: &http.Client{Timeout: 10 * time.Second}}
	be.Handle(protocol.MethodArgoCDRefresh, func(ctx context.Context, _ json.RawMessage) (any, error) {
		return struct{}{}, refresher.Refresh(ctx, argoNS, engine.RepoURL())
	})
	// Argo CD's read-only account; the admin password stays in the cluster.
	handle(protocol.MethodArgoCDCredentials, func(json.RawMessage) string { return "account=" + platform.ViewerUser }, func(ctx context.Context, _ json.RawMessage) (any, error) {
		user, password, err := platform.ViewerCredentials(ctx, k, cfg.Namespace)
		if errors.Is(err, platform.ErrNoViewer) {
			return nil, protocol.Errorf(protocol.CodeFailedPrecondition, "%v: the agent makes it when it applies the platform", err)
		}
		if err != nil {
			return nil, err
		}
		return &protocol.ArgoCDCredentials{Username: user, Password: password}, nil
	})
	reader := inspect.New(k)
	reader.Off, reader.NoLogs, reader.Namespaces = !cfg.KubeRead, !cfg.AppLogs, cfg.ReadNamespaces
	handle(protocol.MethodAppSecretsCreate, describeSecrets, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req protocol.AppSecretsCreate
		if json.Unmarshal(raw, &req) != nil {
			return nil, protocol.Errorf(protocol.CodeBadRequest, "Invalid app secrets payload")
		}
		return struct{}{}, appsecrets.Create(ctx, k, req)
	})
	handle(protocol.MethodKubeRead, audit.Field("op", "kind", "namespace", "name", "container"), func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req protocol.KubeReadRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return reader.Read(ctx, req)
	})
	be.Handle(protocol.MethodAppEvents, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req protocol.AppEventsRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return watcher.Events(ctx, req.App)
	})
	be.Handle(protocol.MethodAppLogs, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if !cfg.AppLogs {
			return nil, protocol.Errorf(protocol.CodeFailedPrecondition, "the people who run this cluster turned off pod logs in Ladder (LADDER_APP_LOGS=off)")
		}
		var req protocol.AppLogsRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return watcher.Logs(ctx, req)
	})
	handle(protocol.MethodAgentUpgrade, audit.Field("version"), func(reqCtx context.Context, raw json.RawMessage) (any, error) {
		var req protocol.AgentUpgradeRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return upgrader.Start(reqCtx, ctx, req.Version)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !be.Ready() {
			http.Error(w, "not connected to the cloud yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})

	go watcher.Run(ctx)
	go serveHTTP(ctx, cfg.HTTPAddr, mux, log)
	be.Run(ctx)
	log.Info("shutting down")
	return nil
}

// describeSecrets names an app env Secret version and how many keys it has,
// never its values.
func describeSecrets(payload json.RawMessage) string {
	var req protocol.AppSecretsCreate
	if json.Unmarshal(payload, &req) != nil {
		return ""
	}
	return fmt.Sprintf("app=%s namespace=%s name=%s keys=%d", req.App, req.Namespace, req.Name, len(req.Values))
}

// signaturePolicy is what an image must carry before the agent runs it
// (spec §5.9): a signature by Ladder's release workflow, unless the people
// who run the cluster turned it off or the agent's own image is in no
// registry, as install-local.sh imports it into k3d.
func signaturePolicy(cfg config.Config, noRegistry bool) imagesig.Policy {
	off := cfg.Signature == "off" || cfg.Signature == "" && noRegistry
	return imagesig.Policy{Off: off, Issuer: cmp.Or(cfg.SignerIssuer, imagesig.DefaultIssuer), Signer: cmp.Or(cfg.Signer, imagesig.DefaultSigner)}
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

// serveHTTP serves /healthz and /readyz.
func serveHTTP(ctx context.Context, addr string, mux *http.ServeMux, log *slog.Logger) {
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
