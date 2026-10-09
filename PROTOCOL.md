# Agent ↔ backend protocol

The agent dials the backend; the backend never connects to the cluster. Types live in
[internal/protocol/protocol.go](internal/protocol/protocol.go), which is the source of truth.

## Connection

- WebSocket, text frames, one JSON envelope per frame.
- The agent connects to `LADDER_BE_URL` (for example `ws://ladder-be.ladder.svc/agent/connect`)
  with `Authorization: Bearer <LADDER_AGENT_TOKEN>`. The backend rejects a bad token with `401`.
- The agent pings every 20 s and reconnects with backoff from 1 s to 30 s.
- Frames can be up to 16 MiB, because `setup.apply` carries the starter repository.

## Envelope

```json
{ "id": "7f3a…", "kind": "request", "method": "cluster.info", "payload": {…} }
{ "id": "7f3a…", "kind": "response", "method": "cluster.info", "payload": {…} }
{ "id": "7f3a…", "kind": "response", "method": "setup.apply", "error": { "code": "bad_request", "message": "…" } }
{ "kind": "event", "method": "setup.status", "payload": {…} }
```

- The backend sends `request`s with a unique `id` and matches each `response` by `id`.
- The agent sends `event`s on its own. Events sent while disconnected are dropped. That is safe,
  because every connect starts with fresh snapshots.
- Error codes: `bad_request`, `unknown_method`, `conflict`, `failed_precondition`, `internal`.

## Requests (backend → agent)

| Method | Payload | Result |
|---|---|---|
| `cluster.info` | none | `ClusterInfo`: version, nodes, allocatable capacity, scheduled pods' requests, limits and free capacity, live `used` CPU and memory when Metrics Server serves them, storage classes, `setupAccess`, Argo CD detection with its Ingress's `address`, all its `addresses`, the `hosts` of its rules and its `tlsHosts`, `warnings`; see [Argo CD's Ingress](#argo-cds-ingress) |
| `apps.status` | none | `AppsSnapshot` |
| `setup.plan` | `SetupPlan` without `files` | `SetupPlanResult`: `repoURL`, Argo CD settings with defaults filled in, steps, cluster objects. No side effects. |
| `setup.apply` | `SetupPlan` with `files` | `SetupStatus`; setup continues in the background |
| `setup.status` | none | `SetupStatus` |
| `setup.retry` | none | `SetupStatus`; continues from the failed step |
| `argocd.refresh` | none | `{}` once Argo CD is asked to read git; see [Refresh](#refresh) |
| `argocd.credentials` | none | `{ username, password }` from Secret `argocd-initial-admin-secret` |
| `agent.upgrade` | `{ version }` | `AgentUpgrade` with `state: running`; see [Upgrade](#upgrade) |
| `kube.read` | `KubeReadRequest` | `KubeReadResult`: `text` as kubectl prints it; see [Cluster reads](#cluster-reads) |
| `app.events` | `{ app }` | `AppEvents`: the events of one app's objects; see [App events and logs](#app-events-and-logs) |
| `app.logs` | `AppLogsRequest` | `AppLogs`: the app's pods and their last lines; see [App events and logs](#app-events-and-logs) |

`SetupPlan`:

```json
{
  "provider": "ladder",
  "owner": "acme",
  "repo": "prod",
  "repoURL": "https://ladder.example.com/git/acme/prod.git",
  "writeMode": "commit",
  "argocd": { "namespace": "argocd", "exposure": "ingress" },
  "files": { "ladder.yaml": "…", "bootstrap/root.yaml": "…", "platform/argocd/values.yaml": "…" },
  "credentials": { "username": "ladder-c-1f2e3d4c5b6a7980", "password": "…" }
}
```

- `ladder`: the repository lives in the Gitea next to the cloud. `owner`, `repo` and `repoURL`
  (the address the cluster clones from) are required; `apply` also needs `credentials` with the
  `username` and `password` (a token that reads only this repository) Argo CD clones with. The
  backend creates the repository and commits the starter files before `apply`. The agent never
  writes to git.
- `github`: `owner` and `repo` are required. `apply` also needs `credentials`
  (`githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`) for Argo CD's repository Secret.
  The backend must create the repository and push the starter files itself, before `apply`.
- `credentials` are kept in Secret `ladder-setup-credentials`, never in the state ConfigMap.
  Step `repo-secret` counts as done only when Secret `repo-deploy` holds this plan's URL and
  credentials, so `apply` with a new token rewrites it.
- Defaults: `writeMode: commit`, `argocd.exposure: ingress` (an Ingress on path `/argocd`, declared
  in `platform/argocd/values.yaml`; `host` is optional), Argo CD version and chart version from the
  agent's configuration.
- Starter files are rendered by the backend. Call `setup.plan` first to learn `repoURL` and the Argo
  CD versions. Required files: `ladder.yaml`, `bootstrap/root.yaml`, `platform/argocd/values.yaml`.
- The agent always installs its own Argo CD. The first step, `check-cluster`, fails when the
  cluster already runs an Argo CD that Ladder did not install (its namespace lacks the label
  `app.kubernetes.io/managed-by=ladder`).
- `apply` returns `conflict` while setup is running and `failed_precondition` once it is done.
  After a failure, either `retry` or `apply` again with a corrected plan. Steps that are already done
  are skipped.
- A step that fails because the cluster's API did not answer (dropped connection, timeout, `429`,
  `5xx`) runs again by itself with a growing pause until its timeout. `argocd-install` removes an
  `argocd` release that an earlier attempt left failed or pending in Ladder's namespace, then
  installs again.

## Cluster reads

`kube.read` lets the cloud's assistant look at the cluster. It only reads: it has no way to
change, create, delete or exec anything.

```json
{ "op": "list", "kind": "pods", "namespace": "shop", "labelSelector": "app.kubernetes.io/instance=shop-api" }
{ "text": "NAME            READY   STATUS             RESTARTS   AGE\nshop-api-7d9f   0/1     ImagePullBackOff   0          6m" }
```

| `op` | Needs | Text |
|---|---|---|
| `list` | `kind`; `namespace` (empty: all namespaces), `labelSelector` | The columns the API server prints for the kind (`kubectl get`), the namespace first for all namespaces; at most 200 rows |
| `get` | `kind`, `name`; `namespace` for a namespaced kind | The object as YAML (`kubectl get -o yaml`), with its status |
| `events` | `namespace` (empty: all); `kind` and `name` for one object | Events oldest first, the newest 60: last seen, type, reason, object, message (`kubectl events`) |
| `logs` | `namespace`, `name` of a pod; `container` (empty: the first), `previous`, `tailLines` (default 100, at most 1000) | The last lines of the container (`kubectl logs`), after a line naming the pod's containers |

- `kind` is a kind, resource or short name as kubectl takes them: `Pod`, `deployments`, `deploy`,
  `applications.argoproj.io`. A kind the cluster does not serve, a missing object or a bad selector is
  `bad_request`, with the API server's message.
- **Secrets stay in the cluster.** `get` of a Secret replaces each value of `data` and `stringData`
  with `<hidden, N bytes>`. Every object loses `metadata.managedFields` and the annotation
  `kubectl.kubernetes.io/last-applied-configuration`, which holds a copy of a Secret's values when
  it was applied with kubectl. `list` prints only the server's columns, which hold no values.
- `text` is at most 32 KiB; `truncated` is set when it was cut. Logs keep their newest lines.
- Agents before this method answer `unknown_method`.

## App events and logs

`app.events` and `app.logs` feed the Events and Logs tabs of the app page. Unlike `kube.read`
they answer in fields, not text, and reach only the objects of one app: its Application (`app` is
its name) and the app's tree, the same as for `problems` below. Both only read.

```json
{ "app": "shop-api" }
{ "events": [ { "type": "Warning", "reason": "BackOff", "kind": "Pod", "name": "shop-api-7d9f8-x2kqp",
    "message": "Back-off restarting failed container web", "count": 7,
    "firstSeen": "2026-10-06T12:30:00Z", "lastSeen": "2026-10-06T12:39:00Z" } ] }
```

- `app.events` returns the events, Normal and Warning, of the Application (Argo CD's syncs and
  changes of health) and of every object in the tree, newest first, at most 200 (`truncated` when
  there were more). A pod that left the tree, such as one a finished rollout deleted, still counts
  while its events last: a pod whose name is that of a ReplicaSet, Job or DaemonSet of the tree and
  the five characters they add. `count` is how many times it happened between `firstSeen` and
  `lastSeen`. Kubernetes keeps events for about an hour.

```json
{ "app": "shop-api", "pod": "", "container": "", "previous": false, "tailLines": 200 }
{ "pods": [ { "name": "shop-api-7d9f8-x2kqp", "status": "CrashLoopBackOff", "ready": false, "restarts": 7,
    "containers": ["web"], "node": "k3d-dev-server-0", "created": "2026-10-06T12:29:58Z" } ],
  "lines": [ { "pod": "shop-api-7d9f8-x2kqp", "container": "web", "time": "2026-10-06T12:39:01.5Z",
    "text": "Error: DATABASE_URL is not set" } ],
  "errors": [ { "pod": "shop-api-7d9f8-q8z2n", "container": "web",
    "message": "container \"web\" in pod \"shop-api-7d9f8-q8z2n\" is waiting to start: ContainerCreating" } ] }
```

- `pods` are the pods of the tree, newest first, including those that finished or are being
  deleted. `status` is the STATUS column of `kubectl get pods`; `restarts` counts every container.
- `lines` are the last `tailLines` lines of each container read (default 200, at most 1000), oldest
  first, with the time the kubelet recorded for each. With `pod` they are that pod's; without it,
  those of the 10 newest pods, merged by time. `container` empty is each pod's first container; with
  `pod` it may also be an init container, without it pods that lack the container are skipped.
  `previous` reads the run before the last restart, and without `pod` skips the pods whose container
  never restarted.
- `errors` names the containers whose lines the API server refused to read, with its message, such
  as a container that has not started.
- A container's read stops at 1 MiB and all lines at 2 MiB, keeping the newest; `truncated` says
  that lines were left out.
- Errors: `failed_precondition` while Argo CD has no Application of that name or the agent does not
  watch Argo CD yet; `bad_request` for a pod that is not the app's or a container its pod lacks.
  Agents before these methods answer `unknown_method`.

## Events (agent → backend)

| Event | When | Payload |
|---|---|---|
| `agent.hello` | first message after every connect | `agentVersion`, `clusterID` (UID of `kube-system`), `context`, `kubernetesVersion` |
| `setup.status` | after connect and on every step change | `SetupStatus` |
| `apps.snapshot` | after connect, and once the Argo CD watch starts | `AppsSnapshot` (`watching: false` until the Application CRD exists) |
| `apps.changed` | an Application's sync, health, revision, operation phase, deletion, problems, pods or Ingresses changed | `{ app: AppStatus, deleted?: true }` |
| `agent.upgrade` | an upgrade starts or fails, and after a connect while one is known | `AgentUpgrade`: `state` (`running`, `failed`), `from`, `to`, `image`, `error` |

`SetupStatus.state` is one of `not_started`, `running`, `done` or `failed`. A step's `status` is
one of `pending`, `running`, `done`, `skipped` (for example, Argo CD install when it is reused) or
`failed`. Each step carries `command` (the equivalent manual command), `error` and `detail`
(on failure: pod problems, warning events or Application conditions).

`AppStatus` covers Applications with label `ladder.dev/managed=true`: `name`, `namespace`
(destination), `sync`, `health`, `revision`, `operationPhase`, `message`, `problems`, `deleting`,
`pods` and `ingresses`.
`deleting` is true once the Application has a `deletionTimestamp`: the ApplicationSet deleted it
because its folder is gone, and its finalizer `resources-finalizer.argocd.argoproj.io` keeps it
until Argo CD has deleted the app's objects. Then `apps.changed` comes with `deleted: true`.
`message` is what Argo CD reports as wrong, one problem per line: conditions whose type ends in
`Error` (`ComparisonError`, `SyncError`, ...) and the message of a sync operation that failed or is
being retried. It is empty when nothing is wrong. While the Application is being deleted, it holds
only `DeletionError` conditions: with the app's folder gone, comparing with git fails by design.
`problems` is what Kubernetes reports as wrong with the app's
objects, of any kind, one per line, oldest first, such as `Pod web-7d9f: FailedScheduling: 0/1
nodes are available: 1 Insufficient cpu.` It holds the Warning events of the app's tree: what
Argo CD manages for the app (`status.resources`) and what those objects own, followed through
ReplicaSets, Jobs and Pods. Objects of one kind with the same problem share a line. A container
waiting to restart adds how it last exited, such as `OOMKilled`, which no event says. Problems
are read only while the app is not `Healthy`, and only from events seen since it last left
`Healthy`, so a rollout does not show the warnings of an earlier one. Argo CD shows these
problems only in its resource tree, not in the Application. `pods` is `{ ready, total }`: the pods
in the same tree that run or start, and how many of them are ready. Pods that finished, such as a
Job's, and pods being deleted, such as those of a rollout's old ReplicaSet, do not count. Agents
before this field leave it out.
`ingresses` are the Ingresses Argo CD manages for the app (`status.resources`), by name:
`{ name, hosts, addresses }`, where `hosts` are those of the rules and `addresses` the IPs and host
names in the Ingress's `status.loadBalancer.ingress`, which the ingress controller sets; absent
until it has. A change of either sends `apps.changed`. The cloud tells people where to point the
hosts in DNS from them. Absent for an app without an Ingress and from agents before this field;
the cloud then falls back to `cluster.info`'s `argocd.addresses`, those of the default ingress
class, and to `argocd.address` from agents before that. An app that exists in git but has no Application is the backend's
to show as `Missing`.

## Argo CD's Ingress

`cluster.info` reads Ingress `argocd-server` in Argo CD's namespace. `address` and `addresses` are
the IPs and host names in its `status.loadBalancer.ingress`; `hosts` are the hosts of its rules,
`[]` while its rule takes every host; `tlsHosts` are the hosts of its `tls` entries whose Secret
holds a `tls.crt`, which cert-manager writes once Let's Encrypt has issued the certificate. The
cloud links Argo CD over HTTPS at a TLS host, over HTTP at another host, and at an address
without one. All four are absent or `null` without the Ingress, and `hosts` and `tlsHosts` from
agents before them: the cloud gives Argo CD its sslip.io host only when `hosts` is `[]` (spec §5.6).

## Upgrade

`agent.upgrade` runs `version` as a tag of the image repository the agent runs from: the cloud
cannot pick another image. The agent sets the tag in its Deployment, together with what an older
install lacks: `RollingUpdate` with `maxUnavailable: 0`, the readiness probe `/readyz`, and
`POD_NAME`. The new pod is ready once it has connected to the cloud, so the old agent keeps running
until then. When the new agent connects, the backend drops the old connection with
`StatusPolicyViolation`; an agent whose connection is taken waits 30 s before it reconnects, so
Kubernetes can stop it. The upgrade is done when an agent of the new version sends `agent.hello`.

The old agent fails the upgrade when a new pod's image does not pull, its container cannot be
created, it exits, or it is not ready within 5 minutes. It then sets its own image back and pushes
`agent.upgrade` with `state: failed` and `error`. Errors: `bad_request` for a tag that is not valid,
`failed_precondition` for the version that runs already, `conflict` while setup or another
upgrade runs. A new agent resumes an interrupted setup only once it is the Deployment's only pod.

## Refresh

The cloud's Gitea posts every push to `main` to the cloud, which sends `argocd.refresh` to the
cluster's agent. The agent annotates every ApplicationSet with
`argocd.argoproj.io/application-set-refresh` and every Application with
`argocd.argoproj.io/refresh`, so Argo CD reads git at once instead of at its next poll, about 3
minutes later. An agent that does not know the method answers `unknown_method`, and Argo CD reads
the push at its next poll.
