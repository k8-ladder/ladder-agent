# ladder-agent

The in-cluster part of Ladder (POC). It runs in namespace `ladder`, dials `ladder-be` over WebSocket
and does everything that touches Kubernetes:

- **cluster facts** for the cluster screen: version, nodes, free capacity, storage classes, access,
  Argo CD detection with its Ingress address;
- **Argo CD status:** a watch on Applications labelled `ladder.dev/managed=true`;
- **setup engine** (spec 5.4): a cluster check, Argo CD through the Helm SDK with its UI on
  `/argocd`, the Secret Argo CD reads the repository in the cloud's Gitea with, root Application,
  waiting for sync. Progress is kept in ConfigMap `ladder-setup-state`, so a restarted pod resumes;
- **refresh:** on `argocd.refresh`, sent by the cloud when `main` gets a push, Argo CD reads git at
  once;
- **Argo CD credentials:** the initial admin password, on request;
- **cluster reads for the assistant:** `kube.read` lists objects as `kubectl get` does, prints one
  as YAML, and reads events and pod logs. It never changes anything, and hides the values of
  Secrets.

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
| `internal/inspect` | `kube.read`: objects, events and logs for the assistant |
| `internal/setup` | setup engine, steps, state |
| `internal/helmx` | Helm SDK wrapper |
| `deploy/` | dev manifests for k3d, pointing at fakebe (the real install is `install.sh` from the cloud) |

## Run on k3d

```bash
make fakebe            # terminal 1: backend stand-in on :9000
make up                # build the image, import it into k3d, apply deploy/, restart
make logs

curl localhost:9000/cluster
# The repository must exist with the starter files: the cloud commits them before setup.apply.
curl -X POST localhost:9000/setup/plan  -d '{"provider":"ladder","owner":"acme","repo":"dev","repoURL":"http://host.k3d.internal:8081/git/acme/dev.git"}'
curl -X POST localhost:9000/setup/apply -d '{"provider":"ladder","owner":"acme","repo":"dev","repoURL":"http://host.k3d.internal:8081/git/acme/dev.git","credentials":{"username":"<user>","password":"<token>"}}'
curl localhost:9000/setup/status        # fakebe also prints every step change
curl localhost:9000/apps
curl localhost:9000/argocd/credentials
curl -X POST localhost:9000/argocd/refresh
curl -X POST localhost:9000/kube -d '{"op":"events","namespace":"argocd"}'
curl -X POST localhost:9000/app/events -d '{"app":"shop-api"}'
curl -X POST localhost:9000/app/logs -d '{"app":"shop-api","tailLines":50}'

make reset             # remove Argo CD and setup state, restart the agent
```

The agent in the cluster reaches fakebe at `host.k3d.internal:9000`.

`make run` starts the agent on the laptop against the current kube context: cluster facts, the
watcher and setup work as in the cluster.

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
| `HTTP_ADDR` | `:8081` | `/healthz` and `/readyz` |
| `LOG_LEVEL` | `info` | |

## Known gaps in the POC

- The agent only creates Argo CD's repository Secret. Creating the repository and committing the
  starter files is the backend's job, before `setup.apply`.
- A Helm release left `pending-install` by a killed pod stops the Argo CD step with a manual hint.
- The watcher picks the Argo CD namespace at startup. Setting up into a non-default namespace needs
  an agent restart to watch it.
- Unit tests cover the sync check of setup, the watcher, cluster reads, logs and the upgrade.
