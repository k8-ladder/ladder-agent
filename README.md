# ladder-agent

The in-cluster part of Ladder (POC). It runs in namespace `ladder`, dials `ladder-be` over WebSocket
and does everything that touches Kubernetes:

- **cluster facts** for the cluster screen: version, nodes, free capacity, storage classes, access,
  Argo CD detection with its Ingress address;
- **Argo CD status:** a watch on Applications labelled `ladder.dev/managed=true`;
- **setup engine** (spec 5.4): a cluster check, Gitea, Argo CD through the Helm SDK with its UI on
  `/argocd`, repository Secret, root Application, waiting for sync. Progress is kept in ConfigMap
  `ladder-setup-state`, so a restarted pod resumes;
- **Gitea proxy:** `git.http` forwards the cloud's calls to the Gitea API of the repository and adds
  the token, which never leaves the cluster;
- **Argo CD credentials:** the initial admin password, on request.

Git hosting, YAML edits and the API for the UI belong to `ladder-be`. The wire contract is in
[PROTOCOL.md](PROTOCOL.md).

## Layout

| Path | What |
|---|---|
| `cmd/agent` | the agent |
| `cmd/fakebe` | a stand-in backend for development; renders a minimal starter repository from `cmd/fakebe/starter` |
| `internal/protocol` | wire types |
| `internal/link` | WebSocket client: reconnect, request dispatch, events |
| `internal/cluster` | cluster facts |
| `internal/argo` | Application watcher |
| `internal/setup` | setup engine, steps, state |
| `internal/gitea` | Gitea manifests and API client |
| `internal/helmx` | Helm SDK wrapper |
| `deploy/` | dev manifests for k3d, pointing at fakebe (the real install is `install.sh` from the cloud) |

## Run on k3d

```bash
make fakebe            # terminal 1: backend stand-in on :9000
make up                # build the image, import it into k3d, apply deploy/, restart
make logs

curl localhost:9000/cluster
curl -X POST localhost:9000/setup/plan  -d '{"provider":"local"}'
curl -X POST localhost:9000/setup/apply -d '{"provider":"local"}'   # fakebe adds the starter files
curl localhost:9000/setup/status        # fakebe also prints every step change
curl localhost:9000/apps
curl localhost:9000/argocd/credentials
curl -X POST localhost:9000/git -d '{"method":"GET","path":"/api/v1/repos/ladder/deploy/contents/ladder.yaml"}'

make reset             # remove Argo CD, Gitea and setup state, restart the agent
```

The agent in the cluster reaches fakebe at `host.k3d.internal:9000`.

`make run` starts the agent on the laptop against the current kube context. That is enough for
cluster facts, the watcher and Argo CD steps. The Gitea API is reachable only inside the cluster,
so Local mode then needs `kubectl -n ladder-git port-forward svc/gitea-http 3000`
(`LADDER_GITEA_URL=http://localhost:3000`) and a `setup/retry` once Gitea is up.

## Configuration

| Variable | Default | |
|---|---|---|
| `LADDER_BE_URL` | (required) | backend WebSocket URL, from Secret `ladder-agent` key `url` |
| `LADDER_AGENT_TOKEN` | (required) | bearer token, from Secret `ladder-agent` key `token` |
| `POD_NAMESPACE` | `ladder` | where the state ConfigMap and Secrets live |
| `ARGOCD_NAMESPACE` | `argocd` | |
| `ARGOCD_DEFAULT_VERSION` | `3.5.3` | |
| `ARGOCD_CHART_VERSION` | `10.9.6` | the argo-cd chart that ships Argo CD 3.5.3 |
| `ARGOCD_CHART_REPO` | `https://argoproj.github.io/argo-helm` | |
| `GITEA_IMAGE` | `gitea/gitea:1.27.3-rootless` | |
| `LADDER_GITEA_URL` | in-cluster service | override for running outside the cluster |
| `HEALTH_ADDR` | `:8081` | `/healthz` |
| `LOG_LEVEL` | `info` | |

## Known gaps in the POC

- GitHub mode: the agent only creates Argo CD's repository Secret. Creating the repository and
  pushing the starter files is the backend's job, before `setup.apply`.
- A Helm release left `pending-install` by a killed pod stops the Argo CD step with a manual hint.
- The watcher picks the Argo CD namespace at startup. Setting up into a non-default namespace needs
  an agent restart to watch it.
- Unit tests cover only the Gitea proxy's path check.
