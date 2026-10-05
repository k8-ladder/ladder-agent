// Package protocol is the wire contract between ladder-agent and ladder-be.
//
// The agent dials the backend over WebSocket and exchanges JSON envelopes.
// The backend sends requests, the agent answers with responses and pushes
// events on its own. See PROTOCOL.md for the full description.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

type Kind string

const (
	KindRequest  Kind = "request"
	KindResponse Kind = "response"
	KindEvent    Kind = "event"
)

// Envelope is one WebSocket text message.
type Envelope struct {
	// ID correlates a response with its request. Empty for events.
	ID      string          `json:"id,omitempty"`
	Kind    Kind            `json:"kind"`
	Method  string          `json:"method,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Requests from the backend to the agent.
const (
	MethodClusterInfo = "cluster.info"
	MethodAppsStatus  = "apps.status"
	MethodSetupPlan   = "setup.plan"
	MethodSetupApply  = "setup.apply"
	MethodSetupStatus = "setup.status"
	MethodSetupRetry  = "setup.retry"
	// MethodGitHTTP forwards one call to the Gitea API of the GitOps repository.
	MethodGitHTTP = "git.http"
	// MethodArgoCDCredentials reads the initial Argo CD admin password.
	MethodArgoCDCredentials = "argocd.credentials"
)

// Events from the agent to the backend.
const (
	EventHello        = "agent.hello"
	EventSetupStatus  = "setup.status"
	EventAppsSnapshot = "apps.snapshot"
	EventAppsChanged  = "apps.changed"
)

// Error codes.
const (
	CodeBadRequest         = "bad_request"
	CodeUnknownMethod      = "unknown_method"
	CodeConflict           = "conflict"
	CodeFailedPrecondition = "failed_precondition"
	CodeInternal           = "internal"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsError converts any error into a wire error, keeping the code of a *Error.
func AsError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// Hello is the first event after every (re)connect.
type Hello struct {
	AgentVersion      string `json:"agentVersion"`
	ClusterID         string `json:"clusterID"`
	Context           string `json:"context"`
	KubernetesVersion string `json:"kubernetesVersion"`
}

// ClusterInfo answers cluster.info: facts for the cluster screen.
type ClusterInfo struct {
	Context           string         `json:"context"`
	KubernetesVersion string         `json:"kubernetesVersion"`
	Nodes             []Node         `json:"nodes"`
	Allocatable       Resources      `json:"allocatable"`
	Requested         Resources      `json:"requested"`
	Free              Resources      `json:"free"`
	StorageClasses    []StorageClass `json:"storageClasses"`
	SetupAccess       bool           `json:"setupAccess"`
	ArgoCD            ArgoCDInfo     `json:"argocd"`
	// Warnings lists facts the agent could not read, usually for lack of RBAC.
	Warnings []string `json:"warnings,omitempty"`
}

type Node struct {
	Name           string    `json:"name"`
	Ready          bool      `json:"ready"`
	Roles          []string  `json:"roles"`
	KubeletVersion string    `json:"kubeletVersion"`
	Allocatable    Resources `json:"allocatable"`
}

type Resources struct {
	CPUMillis   int64 `json:"cpuMillis"`
	MemoryBytes int64 `json:"memoryBytes"`
}

type StorageClass struct {
	Name        string `json:"name"`
	Provisioner string `json:"provisioner"`
	Default     bool   `json:"default"`
}

type ArgoCDInfo struct {
	Found     bool   `json:"found"`
	CRD       bool   `json:"crd"`
	Namespace string `json:"namespace,omitempty"`
	Version   string `json:"version,omitempty"`
	// Address is the IP or host name in the status of Ingress argocd-server,
	// once the ingress controller has set it.
	Address string `json:"address,omitempty"`
}

// AppStatus is the Argo CD view of one Ladder app (an Application with
// label ladder.dev/managed=true).
type AppStatus struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	Sync           string `json:"sync"`
	Health         string `json:"health"`
	Revision       string `json:"revision"`
	OperationPhase string `json:"operationPhase,omitempty"`
	// Message is Argo CD's error, if any: error conditions and a sync
	// operation that failed or is being retried. Lines are separated by "\n".
	Message string `json:"message,omitempty"`
	// Problems is what Kubernetes reports as wrong with the app's objects
	// while the app is not Healthy: the Warning events of what Argo CD
	// manages for it and of what those own, one per line, oldest first.
	Problems string `json:"problems,omitempty"`
}

// AppsSnapshot answers apps.status and is pushed as apps.snapshot.
type AppsSnapshot struct {
	// Watching is false until the Application CRD exists and the cache is synced.
	Watching bool        `json:"watching"`
	Apps     []AppStatus `json:"apps"`
}

// AppsChanged is pushed when one Application changes or disappears.
type AppsChanged struct {
	App     AppStatus `json:"app"`
	Deleted bool      `json:"deleted,omitempty"`
}

// Setup providers, write modes and states.
const (
	ProviderLocal  = "local"
	ProviderGitHub = "github"

	WriteModePR     = "pr"
	WriteModeCommit = "commit"

	SetupNotStarted = "not_started"
	SetupRunning    = "running"
	SetupDone       = "done"
	SetupFailed     = "failed"

	StepPending = "pending"
	StepRunning = "running"
	StepDone    = "done"
	StepSkipped = "skipped"
	StepFailed  = "failed"
)

// SetupPlan is the body of setup.plan and setup.apply.
type SetupPlan struct {
	Provider  string     `json:"provider"`
	Owner     string     `json:"owner,omitempty"`
	Repo      string     `json:"repo"`
	RepoURL   string     `json:"repoURL,omitempty"`
	WriteMode string     `json:"writeMode"`
	ArgoCD    ArgoCDPlan `json:"argocd"`
	// Files is the starter repository rendered by the backend, path → content.
	// Required by setup.apply; setup.plan ignores it.
	Files map[string]string `json:"files,omitempty"`
	// Credentials let Argo CD read a GitHub repository. Never stored in the
	// state ConfigMap.
	Credentials *RepoCredentials `json:"credentials,omitempty"`
}

type ArgoCDPlan struct {
	Version      string `json:"version,omitempty"`
	ChartVersion string `json:"chartVersion,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	Exposure     string `json:"exposure,omitempty"` // ingress | port-forward
	// Host of the Ingress; empty means any host, on path /argocd.
	Host string `json:"host,omitempty"`
}

type RepoCredentials struct {
	GitHubAppID             int64  `json:"githubAppID"`
	GitHubAppInstallationID int64  `json:"githubAppInstallationID"`
	GitHubAppPrivateKey     string `json:"githubAppPrivateKey"`
}

// SetupPlanResult answers setup.plan. It has no side effects.
type SetupPlanResult struct {
	RepoURL        string          `json:"repoURL"`
	ArgoCD         ArgoCDPlan      `json:"argocd"`
	Steps          []StepStatus    `json:"steps"`
	ClusterObjects []ClusterObject `json:"clusterObjects"`
}

type ClusterObject struct {
	Action    string `json:"action"` // create
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"`
}

// SetupStatus answers setup.status and is pushed on every step change.
type SetupStatus struct {
	State    string       `json:"state"`
	Provider string       `json:"provider,omitempty"`
	RepoURL  string       `json:"repoURL,omitempty"`
	Steps    []StepStatus `json:"steps"`
}

type StepStatus struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Command string `json:"command"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	// Detail is a short note: "already done", diagnostics on failure, and so on.
	Detail     string `json:"detail,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

// GitHTTPRequest is the payload of git.http. Path starts with /api/v1/repos/
// and must stay inside the GitOps repository; the agent adds the token.
type GitHTTPRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Query  string `json:"query,omitempty"`
	Body   []byte `json:"body,omitempty"`
}

// GitHTTPResponse answers git.http with Gitea's answer, whatever its status.
type GitHTTPResponse struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

// ArgoCDCredentials answers argocd.credentials.
type ArgoCDCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
