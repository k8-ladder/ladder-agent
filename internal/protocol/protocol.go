// Package protocol mirrors the wire contract between ladder-agent and
// ladder-be. ladder-agent/PROTOCOL.md is the contract; the agent's
// internal/protocol package holds the same types. Keep the two in sync.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
	// MethodArgoCDRefresh has Argo CD read git now: the cloud sends it when
	// the repository's main branch gets a push.
	MethodArgoCDRefresh = "argocd.refresh"
	// MethodArgoCDCredentials reads the initial Argo CD admin password.
	MethodArgoCDCredentials = "argocd.credentials"
	// MethodAgentUpgrade replaces the agent with another version of its image.
	MethodAgentUpgrade = "agent.upgrade"
	// MethodKubeRead reads cluster objects, events and logs for the
	// assistant. It never changes anything and never returns Secret values.
	MethodKubeRead = "kube.read"
	// MethodAppEvents reads the events of one app's objects for its page.
	MethodAppEvents = "app.events"
	// MethodAppLogs reads the last lines of one app's pods for its page.
	MethodAppLogs = "app.logs"
	// MethodAppSecretsCreate creates an immutable version of an app's env secrets.
	MethodAppSecretsCreate = "app.secrets.create"
)

// AppSecretsCreate never returns values. Name is ladder-<app>-env-<32 hex digits>.
type AppSecretsCreate struct {
	App       string            `json:"app"`
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Values    map[string]string `json:"values"`
}

// Events from the agent to the backend.
const (
	EventHello        = "agent.hello"
	EventSetupStatus  = "setup.status"
	EventAppsSnapshot = "apps.snapshot"
	EventAppsChanged  = "apps.changed"
	// EventAgentUpgrade reports an upgrade from the agent being replaced.
	EventAgentUpgrade = "agent.upgrade"
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

// AgentUpgradeRequest asks the agent to run another version: a tag of the
// image repository it runs from.
type AgentUpgradeRequest struct {
	Version string `json:"version"`
}

// AgentUpgrade answers agent.upgrade and is pushed as agent.upgrade. The
// upgrade is done when an agent of version To says hello.
type AgentUpgrade struct {
	State string `json:"state"` // running | failed
	From  string `json:"from"`
	To    string `json:"to"`
	Image string `json:"image"`
	// Error says why the new version did not start; the agent is back on From.
	Error string `json:"error,omitempty"`
}

// Upgrade states.
const (
	UpgradeRunning = "running"
	UpgradeFailed  = "failed"
)

// ClusterInfo answers cluster.info: facts for the cluster screen.
type ClusterInfo struct {
	Context           string    `json:"context"`
	KubernetesVersion string    `json:"kubernetesVersion"`
	Nodes             []Node    `json:"nodes"`
	Allocatable       Resources `json:"allocatable"`
	Requested         Resources `json:"requested"`
	// Limits are the total CPU and memory limits of scheduled pod containers.
	// Nil means this agent could not read pods.
	Limits *Resources `json:"limits,omitempty"`
	// Used is the live CPU and memory the Metrics API reports for running pod
	// containers. Nil means Metrics Server is unavailable or refused the read.
	Used           *Resources     `json:"used,omitempty"`
	Free           Resources      `json:"free"`
	StorageClasses []StorageClass `json:"storageClasses"`
	SetupAccess    bool           `json:"setupAccess"`
	ArgoCD         ArgoCDInfo     `json:"argocd"`
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
	// Addresses are all of them, Address first: the default ingress class's
	// addresses, where app hosts point. With k3s's servicelb, one per node.
	// Absent from agents that report only Address.
	Addresses []string `json:"addresses,omitempty"`
	// Hosts are the hosts of the Ingress's rules, empty while it takes every
	// host. TLSHosts are those of its TLS entries whose Secret holds a
	// certificate: Argo CD opens over HTTPS there. Both absent without the
	// Ingress and from agents before them: Hosts is sent empty, so that the
	// cloud tells a hostless Ingress from an agent that does not say.
	Hosts    []string `json:"hosts"`
	TLSHosts []string `json:"tlsHosts,omitempty"`
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
	// Deleting is true once the Application is being deleted: Argo CD
	// deletes the app's objects, then the Application.
	Deleting bool `json:"deleting,omitempty"`
	// Pods counts the pods of the app's objects. Absent from agents that do
	// not count them.
	Pods *Pods `json:"pods,omitempty"`
	// Ingresses are the app's Ingresses, by name. Absent from agents that do
	// not read them, and for an app without one.
	Ingresses []Ingress `json:"ingresses,omitempty"`
}

// Ingress is one Ingress that Argo CD manages for an app.
type Ingress struct {
	Name  string   `json:"name"`
	Hosts []string `json:"hosts"`
	// Addresses are the IPs and host names in the Ingress's status, which
	// the ingress controller sets; empty until it has.
	Addresses []string `json:"addresses,omitempty"`
}

// Pods are the pods in an app's tree that run or start: not those that
// finished, such as a Job's, nor those being deleted.
type Pods struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
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

// Setup providers, write modes and states. With ProviderLadder the
// repository lives in the Gitea next to the Ladder cloud, which creates it
// and commits the starter files before setup.apply.
const (
	ProviderLadder = "ladder"
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
	// Traefik has setup add Traefik and cert-manager, in a cluster Ladder
	// created in a cloud whose clusters come without an ingress controller.
	Traefik bool `json:"traefik,omitempty"`
	// MetricsServer is set by the agent, whatever the cloud sends: whether
	// setup adds Metrics Server, which it does for a cluster without a
	// Metrics API.
	MetricsServer *bool `json:"metricsServer,omitempty"`
	// Files is the starter repository of clouds from before the agent made
	// the platform. Ignored; kept in saved plans, where it tells an older
	// setup's Traefik and Metrics Server.
	Files map[string]string `json:"files,omitempty"`
	// Credentials let Argo CD read the repository. Never stored in the state
	// ConfigMap.
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

// RepoCredentials are a user name and token for ProviderLadder, or a
// read-only SSH deploy key or a GitHub App for ProviderGitHub.
type RepoCredentials struct {
	SSHPrivateKey           string `json:"sshPrivateKey,omitempty"`
	Username                string `json:"username,omitempty"`
	Password                string `json:"password,omitempty"`
	GitHubAppID             int64  `json:"githubAppID,omitempty"`
	GitHubAppInstallationID int64  `json:"githubAppInstallationID,omitempty"`
	GitHubAppPrivateKey     string `json:"githubAppPrivateKey,omitempty"`
}

// SetupPlanResult answers setup.plan. It has no side effects.
type SetupPlanResult struct {
	RepoURL        string          `json:"repoURL"`
	ArgoCD         ArgoCDPlan      `json:"argocd"`
	Steps          []StepStatus    `json:"steps"`
	ClusterObjects []ClusterObject `json:"clusterObjects"`
	// MetricsServerPresent reports an existing Metrics API registration, even
	// if unhealthy. Nil means an older agent did not check; do not install over it.
	MetricsServerPresent *bool `json:"metricsServerPresent,omitempty"`
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
	// Platform is where the agent stands with the platform it applies from
	// its image once setup is done, such as after an upgrade.
	Platform *PlatformStatus `json:"platform,omitempty"`
}

// PlatformStatus is the platform the agent applied after setup.
type PlatformStatus struct {
	State string `json:"state"` // applying | applied | failed | needs_access
	// Hash identifies what the agent applies; it changes with the agent's
	// version and with people's overrides.
	Hash  string `json:"hash,omitempty"`
	Error string `json:"error,omitempty"`
	// Command gives the agent the access it waits for, with needs_access.
	Command string `json:"command,omitempty"`
}

const (
	PlatformApplying = "applying"
	PlatformApplied  = "applied"
	PlatformFailed   = "failed"
	// PlatformNeedsAccess: the platform changed, and the agent waits for
	// setup access to apply it; Error holds the command that gives it.
	PlatformNeedsAccess = "needs_access"
)

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

// Operations of kube.read.
const (
	KubeList   = "list"
	KubeGet    = "get"
	KubeEvents = "events"
	KubeLogs   = "logs"
)

// KubeReadRequest is the payload of kube.read: one look at the cluster, as
// kubectl get, kubectl get -o yaml, kubectl events and kubectl logs show it.
type KubeReadRequest struct {
	Op string `json:"op"`
	// Kind is a kind, resource or short name as kubectl takes them: Pod,
	// deployments, deploy, applications.argoproj.io. Required for list and
	// get; for events it narrows them to one object with Name.
	Kind string `json:"kind,omitempty"`
	// Namespace is empty for all namespaces (list, events) and for
	// cluster-scoped objects.
	Namespace     string `json:"namespace,omitempty"`
	Name          string `json:"name,omitempty"`
	LabelSelector string `json:"labelSelector,omitempty"`
	// Container, Previous and TailLines are for logs: the container of the
	// pod Name (empty: its first), the previous run of a restarted one, and
	// how many of the last lines (default 100, at most 1000).
	Container string `json:"container,omitempty"`
	Previous  bool   `json:"previous,omitempty"`
	TailLines int    `json:"tailLines,omitempty"`
}

// KubeReadResult answers kube.read with text as kubectl prints it.
type KubeReadResult struct {
	Text string `json:"text"`
	// Truncated is true when the text was cut at the size limit (32 KiB).
	Truncated bool `json:"truncated,omitempty"`
}

// AppEventsRequest is the payload of app.events.
type AppEventsRequest struct {
	// App is the name of the app's Application.
	App string `json:"app"`
}

// AppEvents answers app.events: the events of the app's Application and of
// the objects in its tree, newest first.
type AppEvents struct {
	Events []AppEvent `json:"events"`
	// Truncated is true when older events were left out (at most 200).
	Truncated bool `json:"truncated,omitempty"`
}

// AppEvent is one Kubernetes event about an object of an app.
type AppEvent struct {
	Type    string `json:"type"` // Normal or Warning
	Reason  string `json:"reason"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Message string `json:"message"`
	// Count is how many times it happened between FirstSeen and LastSeen.
	Count     int       `json:"count"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// AppLogsRequest is the payload of app.logs.
type AppLogsRequest struct {
	// App is the name of the app's Application.
	App string `json:"app"`
	// Pod is one pod of the app; empty reads every pod, the lines merged by time.
	Pod string `json:"pod,omitempty"`
	// Container in each pod; empty is the pod's first container.
	Container string `json:"container,omitempty"`
	// Previous reads the run before the last restart.
	Previous bool `json:"previous,omitempty"`
	// TailLines is how many of the last lines of each pod: default 200, at most 1000.
	TailLines int `json:"tailLines,omitempty"`
}

// AppLogs answers app.logs.
type AppLogs struct {
	// Pods are the app's pods, newest first, whether their lines were read or not.
	Pods []AppPod `json:"pods"`
	// Lines are oldest first.
	Lines []LogLine `json:"lines"`
	// Errors say why the lines of a pod could not be read, such as a
	// container that has not started.
	Errors []LogError `json:"errors,omitempty"`
	// Truncated is true when older lines were left out to keep the answer small.
	Truncated bool `json:"truncated,omitempty"`
}

// AppPod is one pod of an app.
type AppPod struct {
	Name string `json:"name"`
	// Status is what kubectl get pods prints: Running, Pending, Completed,
	// CrashLoopBackOff, ImagePullBackOff, Init:0/1, Terminating and so on.
	Status         string    `json:"status"`
	Ready          bool      `json:"ready"`
	Restarts       int       `json:"restarts"`
	Containers     []string  `json:"containers"`
	InitContainers []string  `json:"initContainers,omitempty"`
	Node           string    `json:"node,omitempty"`
	Created        time.Time `json:"created"`
}

// LogLine is one line a container wrote.
type LogLine struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Time is when the container wrote it, as the kubelet recorded it.
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// LogError says why the lines of a pod's container could not be read.
type LogError struct {
	Pod       string `json:"pod"`
	Container string `json:"container,omitempty"`
	Message   string `json:"message"`
}

// ArgoCDCredentials answers argocd.credentials.
type ArgoCDCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
