// Package setup runs the first-run setup as a resumable background job
// (spec section 5.4): a cluster check, Argo CD, its repository Secret, the
// root Application and the wait for the first sync.
package setup

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ladder/ladder-agent/internal/cluster"
	"github.com/ladder/ladder-agent/internal/config"
	"github.com/ladder/ladder-agent/internal/helmx"
	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/platform"
	"github.com/ladder/ladder-agent/internal/protocol"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Engine struct {
	k         *kube.Clients
	helm      *helmx.Helm
	inspector *cluster.Inspector
	cfg       config.Config
	log       *slog.Logger
	store     *store

	// OnChange receives the status after every change.
	OnChange func(protocol.SetupStatus)

	// jobCtx outlives requests: setup keeps running when the backend disconnects.
	jobCtx context.Context

	mu      sync.Mutex
	plan    *protocol.SetupPlan
	status  protocol.SetupStatus
	running bool
	// platformStarted: StartPlatform runs once per agent.
	platformStarted bool
}

func NewEngine(jobCtx context.Context, k *kube.Clients, h *helmx.Helm, in *cluster.Inspector, cfg config.Config, log *slog.Logger) *Engine {
	return &Engine{
		k: k, helm: h, inspector: in, cfg: cfg, log: log,
		store:  &store{k: k, ns: cfg.Namespace},
		jobCtx: jobCtx,
		status: protocol.SetupStatus{State: protocol.SetupNotStarted, Steps: []protocol.StepStatus{}},
	}
}

// Load reads saved progress. Resume continues a setup that was running when
// the agent stopped.
func (e *Engine) Load(ctx context.Context) error {
	plan, status, err := e.store.load(ctx)
	if err != nil || plan == nil {
		return err
	}
	legacyFlags(plan)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.plan, e.status = plan, *status
	return nil
}

// Resume continues a setup that Load found running. The caller makes sure the
// agent that ran it has stopped.
func (e *Engine) Resume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status.State == protocol.SetupRunning && !e.running {
		e.log.Info("resuming setup")
		e.start()
	}
}

// ArgoCDNamespace is the namespace chosen by setup, if any.
func (e *Engine) ArgoCDNamespace() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.plan == nil {
		return ""
	}
	return e.plan.ArgoCD.Namespace
}

func (e *Engine) Status() protocol.SetupStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.copyStatus()
}

// Plan validates the request and lists steps and cluster objects. No side effects.
func (e *Engine) Plan(ctx context.Context, in protocol.SetupPlan) (*protocol.SetupPlanResult, error) {
	p, err := e.prepare(ctx, in, false)
	if err != nil {
		return nil, err
	}
	// A registered but unhealthy provider still owns the Metrics API. Never
	// replace it just because discovery or a usage reading is unavailable.
	_, err = e.k.Dynamic.Resource(kube.APIServiceGVR).Get(ctx, "v1beta1.metrics.k8s.io", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	metricsPresent := err == nil
	return &protocol.SetupPlanResult{
		RepoURL:              p.RepoURL,
		ArgoCD:               p.ArgoCD,
		Steps:                pendingSteps(e.steps(p)),
		ClusterObjects:       e.clusterObjects(p),
		MetricsServerPresent: &metricsPresent,
	}, nil
}

// Apply starts setup. A failed setup may be applied again with a new plan.
func (e *Engine) Apply(ctx context.Context, in protocol.SetupPlan) (protocol.SetupStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return e.copyStatus(), protocol.Errorf(protocol.CodeConflict, "setup is already running")
	}
	if e.status.State == protocol.SetupDone {
		return e.copyStatus(), protocol.Errorf(protocol.CodeFailedPrecondition, "setup is already complete")
	}
	p, err := e.prepare(ctx, in, true)
	if err != nil {
		return e.copyStatus(), err
	}
	// Metrics Server only where no provider owns the Metrics API, even an
	// unhealthy one; a refused check stops setup before it changes anything.
	_, err = e.k.Dynamic.Resource(kube.APIServiceGVR).Get(ctx, "v1beta1.metrics.k8s.io", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return e.copyStatus(), err
	}
	metricsServer := apierrors.IsNotFound(err)
	p.MetricsServer = &metricsServer
	if err := e.k.EnsureNamespace(ctx, e.cfg.Namespace); err != nil {
		return e.copyStatus(), err
	}
	if err := e.store.saveCredentials(ctx, p.Credentials); err != nil {
		return e.copyStatus(), err
	}
	e.plan = p
	e.status = protocol.SetupStatus{
		State:    protocol.SetupRunning,
		Provider: p.Provider,
		RepoURL:  p.RepoURL,
		Steps:    pendingSteps(e.steps(p)),
	}
	if err := e.store.save(ctx, e.plan, &e.status); err != nil {
		return e.copyStatus(), err
	}
	e.start()
	return e.copyStatus(), nil
}

// Retry continues a failed setup from the failed step.
func (e *Engine) Retry(ctx context.Context) (protocol.SetupStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return e.copyStatus(), protocol.Errorf(protocol.CodeConflict, "setup is already running")
	}
	if e.status.State != protocol.SetupFailed {
		return e.copyStatus(), protocol.Errorf(protocol.CodeFailedPrecondition, "nothing to retry: setup is %s", e.status.State)
	}
	// The failed step runs again, and so does the platform: an agent
	// upgraded since may bring another, which the steps after it wait for.
	for i := range e.status.Steps {
		if s := &e.status.Steps[i]; s.Status == protocol.StepFailed || s.ID == stepPlatformID && s.Status == protocol.StepDone {
			*s = protocol.StepStatus{ID: s.ID, Title: s.Title, Command: s.Command, Status: protocol.StepPending}
		}
	}
	e.status.State = protocol.SetupRunning
	if err := e.store.save(ctx, e.plan, &e.status); err != nil {
		return e.copyStatus(), err
	}
	e.start()
	return e.copyStatus(), nil
}

// start must be called with e.mu held. Saved step statuses are matched to the
// current step list by ID, so a newer agent can resume an older setup.
func (e *Engine) start() {
	saved := map[string]protocol.StepStatus{}
	for _, s := range e.status.Steps {
		saved[s.ID] = s
	}
	steps := pendingSteps(e.steps(e.plan))
	for i, s := range steps {
		if prev, ok := saved[s.ID]; ok {
			prev.Title, prev.Command = s.Title, s.Command
			steps[i] = prev
		}
	}
	e.status.Steps = steps
	e.running = true
	go e.run(e.plan)
}

func (e *Engine) run(plan *protocol.SetupPlan) {
	ctx := e.jobCtx
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()
	for i, s := range e.steps(plan) {
		if st := e.Status().Steps[i].Status; st == protocol.StepDone || st == protocol.StepSkipped {
			continue
		}
		log := e.log.With("step", s.id)
		log.Info("step started")
		e.update(func(st *protocol.SetupStatus) {
			st.Steps[i].Status = protocol.StepRunning
			st.Steps[i].StartedAt = now()
			st.Steps[i].Error, st.Steps[i].Detail = "", ""
		})

		result, detail, err := e.exec(ctx, s)
		if ctx.Err() != nil {
			// The agent is shutting down: leave the step "running" so the
			// next start resumes it.
			log.Info("step interrupted by shutdown")
			return
		}
		if err != nil {
			log.Error("step failed", "err", err)
			e.update(func(st *protocol.SetupStatus) {
				st.Steps[i].Status = protocol.StepFailed
				st.Steps[i].Error = err.Error()
				st.Steps[i].Detail = detail
				st.Steps[i].FinishedAt = now()
				st.State = protocol.SetupFailed
			})
			return
		}
		log.Info("step finished", "status", result, "detail", detail)
		e.update(func(st *protocol.SetupStatus) {
			st.Steps[i].Status = result
			st.Steps[i].Detail = detail
			st.Steps[i].FinishedAt = now()
		})
	}
	e.update(func(st *protocol.SetupStatus) { st.State = protocol.SetupDone })
	e.log.Info("setup complete")
	e.StartPlatform()
}

// StartPlatform keeps the platform of a finished setup as the agent's image
// has it, once per agent: it applies it again when it changed, such as after
// an upgrade or when people changed their overrides, and gives Argo CD its
// host where Ladder added Traefik. It does nothing before setup is done.
func (e *Engine) StartPlatform() {
	e.mu.Lock()
	if e.platformStarted || e.status.State != protocol.SetupDone || e.plan == nil {
		e.mu.Unlock()
		return
	}
	e.platformStarted = true
	plan := *e.plan
	e.mu.Unlock()
	go func() {
		e.reconcilePlatform(e.jobCtx, &plan)
		if plan.Traefik && plan.ArgoCD.Exposure == "ingress" {
			platform.WatchHost(e.jobCtx, e.k, plan.ArgoCD.Namespace, 15*time.Second, e.log.With("component", "argocd-host"))
		}
	}()
}

// reconcilePlatform applies the platform when what the agent last applied
// differs, and tries again with a growing pause while it fails.
func (e *Engine) reconcilePlatform(ctx context.Context, p *protocol.SetupPlan) {
	pause := time.Minute
	for {
		err := e.applyPlatform(ctx, p)
		if err == nil || ctx.Err() != nil {
			return
		}
		e.log.Error("apply the platform", "err", err, "retry in", pause)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		pause = min(2*pause, 15*time.Minute)
	}
}

func (e *Engine) applyPlatform(ctx context.Context, p *protocol.SetupPlan) error {
	params, pl, err := e.render(ctx, p)
	if err == nil {
		var applied string
		if applied, err = platform.AppliedHash(ctx, e.k, e.cfg.Namespace); err == nil && applied == pl.Hash {
			e.setPlatform(protocol.PlatformStatus{State: protocol.PlatformApplied, Hash: pl.Hash})
			return nil
		}
	}
	if err == nil {
		e.log.Info("applying the platform", "hash", pl.Hash)
		e.setPlatform(protocol.PlatformStatus{State: protocol.PlatformApplying, Hash: pl.Hash})
		err = platform.Apply(ctx, e.k, params, pl, e.log, time.Now())
	}
	if err != nil {
		st := protocol.PlatformStatus{State: protocol.PlatformFailed, Error: err.Error()}
		if pl != nil {
			st.Hash = pl.Hash
		}
		e.setPlatform(st)
		return err
	}
	e.log.Info("platform applied", "hash", pl.Hash)
	e.setPlatform(protocol.PlatformStatus{State: protocol.PlatformApplied, Hash: pl.Hash})
	return nil
}

func (e *Engine) setPlatform(st protocol.PlatformStatus) {
	e.mu.Lock()
	same := e.status.Platform != nil && *e.status.Platform == st
	e.mu.Unlock()
	if !same {
		e.update(func(s *protocol.SetupStatus) { s.Platform = &st })
	}
}

func (e *Engine) exec(ctx context.Context, s step) (result, detail string, err error) {
	if s.skip != nil {
		if reason := s.skip(); reason != "" {
			return protocol.StepSkipped, reason, nil
		}
	}
	if s.done != nil {
		ok, err := s.done(ctx)
		if err != nil {
			e.log.Debug("done check failed, running the step", "step", s.id, "err", err)
		}
		if ok {
			return protocol.StepDone, "already done", nil
		}
	}
	sctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	err = e.runRetrying(sctx, s)
	if errors.Is(err, context.DeadlineExceeded) {
		err = errors.New("timed out after " + s.timeout.String())
	}
	if err != nil && s.diagnose != nil && ctx.Err() == nil {
		dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
		detail = s.diagnose(dctx)
		dcancel()
	}
	return protocol.StepDone, detail, err
}

// firstRetryPause is how long runRetrying waits before its first retry.
var firstRetryPause = 2 * time.Second

// runRetrying runs the step, and runs it again with a growing pause while it
// fails because the cluster's API did not answer (kube.Transient), until ctx
// ends. Before running again it checks whether the step is done: the work may
// have gone through although the answer was lost.
func (e *Engine) runRetrying(ctx context.Context, s step) error {
	pause := firstRetryPause
	for {
		err := s.run(ctx)
		if !kube.Transient(err) {
			return err
		}
		e.log.Warn("step failed, trying again", "step", s.id, "err", err, "pause", pause)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(pause):
		}
		pause = min(2*pause, 30*time.Second)
		if s.done != nil {
			if ok, _ := s.done(ctx); ok {
				return nil
			}
		}
	}
}

// update changes the status, saves it and notifies the backend.
func (e *Engine) update(f func(*protocol.SetupStatus)) {
	e.mu.Lock()
	f(&e.status)
	snapshot := e.copyStatus()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := e.store.save(ctx, e.plan, &e.status); err != nil {
		e.log.Error("save setup state", "err", err)
	}
	cancel()
	e.mu.Unlock()
	if e.OnChange != nil {
		e.OnChange(snapshot)
	}
}

func (e *Engine) copyStatus() protocol.SetupStatus {
	s := e.status
	s.Steps = append([]protocol.StepStatus(nil), e.status.Steps...)
	return s
}

func pendingSteps(steps []step) []protocol.StepStatus {
	out := make([]protocol.StepStatus, len(steps))
	for i, s := range steps {
		out[i] = protocol.StepStatus{ID: s.id, Title: s.title, Command: s.command, Status: protocol.StepPending}
	}
	return out
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
