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
| `cluster.info` | none | `ClusterInfo`: version, nodes, allocatable/requested/free, storage classes, `setupAccess`, Argo CD detection with the Ingress `address`, `warnings` |
| `apps.status` | none | `AppsSnapshot` |
| `setup.plan` | `SetupPlan` without `files` | `SetupPlanResult`: `repoURL`, Argo CD settings with defaults filled in, steps, cluster objects. No side effects. |
| `setup.apply` | `SetupPlan` with `files` | `SetupStatus`; setup continues in the background |
| `setup.status` | none | `SetupStatus` |
| `setup.retry` | none | `SetupStatus`; continues from the failed step |
| `git.http` | `GitHTTPRequest` | `GitHTTPResponse`: Gitea's status, content type and body |
| `argocd.credentials` | none | `{ username, password }` from Secret `argocd-initial-admin-secret` |

`SetupPlan`:

```json
{
  "provider": "local",
  "repo": "deploy",
  "writeMode": "commit",
  "argocd": { "namespace": "argocd", "exposure": "ingress" },
  "files": { "ladder.yaml": "…", "bootstrap/root.yaml": "…", "platform/argocd/values.yaml": "…" },
  "credentials": null
}
```

- `local`: the agent fixes the owner to `ladder` and returns
  `repoURL = http://gitea-http.ladder-git.svc:3000/ladder/<repo>.git`.
- `github`: `owner` and `repo` are required. `apply` also needs `credentials`
  (`githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`) for Argo CD's repository Secret.
  The backend must create the repository and push the starter files itself, before `apply`.
- Defaults: `writeMode: commit`, `argocd.exposure: ingress` (an Ingress on path `/argocd`, declared
  in `platform/argocd/values.yaml`; `host` is optional), Argo CD version and chart version from the
  agent's configuration.
- Starter files are rendered by the backend. Call `setup.plan` first to learn `repoURL` and the Argo
  CD versions. Required files: `ladder.yaml`, `bootstrap/root.yaml`, `platform/argocd/values.yaml`.
- The agent always installs its own Argo CD. The first step, `check-cluster`, fails when the
  cluster already runs an Argo CD that Ladder did not install (its namespace lacks the label
  `app.kubernetes.io/managed-by=ladder`), or has no default StorageClass.
- `apply` returns `conflict` while setup is running and `failed_precondition` once it is done.
  After a failure, either `retry` or `apply` again with a corrected plan. Steps that are already done
  are skipped.

## Gitea proxy

`git.http` lets the backend use the Gitea API of the GitOps repository, which is reachable only
inside the cluster.

```json
{ "method": "GET", "path": "/api/v1/repos/ladder/deploy/contents/ladder.yaml", "query": "ref=main" }
{ "status": 200, "contentType": "application/json;charset=utf-8", "body": "eyJ…" }
```

- `body` is base64 in JSON (Go `[]byte`), at most 8 MiB each way.
- `path` is URL-escaped and must be `/api/v1/repos/ladder/<repo>` or below it, with no `.`, `..` or
  empty segments. Methods: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`.
- The agent adds the admin token from Secret `ladder-git/ladder-git-admin`. The token never leaves
  the cluster.
- Gitea's errors come back as a response with their status. A protocol error means the call was
  refused (`bad_request`) or Gitea is not set up yet (`failed_precondition`).

## Events (agent → backend)

| Event | When | Payload |
|---|---|---|
| `agent.hello` | first message after every connect | `agentVersion`, `clusterID` (UID of `kube-system`), `context`, `kubernetesVersion` |
| `setup.status` | after connect and on every step change | `SetupStatus` |
| `apps.snapshot` | after connect, and once the Argo CD watch starts | `AppsSnapshot` (`watching: false` until the Application CRD exists) |
| `apps.changed` | an Application's sync, health, revision or operation phase changed | `{ app: AppStatus, deleted?: true }` |

`SetupStatus.state` is one of `not_started`, `running`, `done` or `failed`. A step's `status` is
one of `pending`, `running`, `done`, `skipped` (for example, Argo CD install when it is reused) or
`failed`. Each step carries `command` (the equivalent manual command), `error` and `detail`
(on failure: pod problems, warning events or Application conditions).

`AppStatus` covers Applications with label `ladder.dev/managed=true`: `name`, `namespace`
(destination), `sync`, `health`, `revision`, `operationPhase` and `message`. `message` is what
Argo CD reports as wrong, one problem per line: conditions whose type ends in `Error`
(`ComparisonError`, `SyncError`, ...) and the message of a sync operation that failed or is being
retried. It is empty when nothing is wrong. An app that exists in git but has no Application is
the backend's to show as `Missing`.
