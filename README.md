# ladder-agent

The in-cluster part of Ladder (POC). It runs in namespace `ladder`, dials the Ladder cloud over
WebSocket and does everything that touches Kubernetes:

- **cluster facts** for the cluster screen: version, nodes, free capacity, storage classes, access,
  Argo CD detection with its Ingress address;
- **Argo CD status:** a watch on Applications labelled `ladder.dev/managed=true`;
- **setup engine**: a cluster check, Argo CD through the Helm SDK with its UI on
  `/argocd`, the Secret Argo CD reads the repository in the cloud's Gitea with, root Application,
  waiting for sync. Progress is kept in ConfigMap `ladder-setup-state`, so a restarted pod resumes;
- **refresh:** on `argocd.refresh`, sent by the cloud when `main` gets a push, Argo CD reads git at
  once;
- **Argo CD credentials:** the initial admin password, on request;
- **cluster reads for the assistant:** `kube.read` lists objects as `kubectl get` does, prints one
  as YAML, and reads events and pod logs. It never changes anything, and hides the values of
  Secrets.

Git hosting, YAML edits and the API for the UI belong to the cloud, which is not in this
repository. The cloud never connects to the cluster: the agent dials it, and refuses what the
people who run the cluster turned off. The wire contract is in [PROTOCOL.md](PROTOCOL.md); the cloud
imports its types from `protocol`.

## Releases

Every push to `main` that changes the agent runs [release.yml](.github/workflows/release.yml): the
tests, then the image for `linux/amd64` and `linux/arm64` as
`ghcr.io/k8-ladder/ladder-agent:<version>` (the build time, such as `20261124112235`), signed
with cosign keyless and only then tagged `latest`, then the release `agent-<version>`:

| File | What |
|---|---|
| `install.sh` | installs the agent into the cluster of the current kubectl context; takes `LADDER_URL` and `LADDER_INSTALL_URL` |
| `agent.yaml` | the agent's manifest, with the image by digest |
| `*.sigstore.json` | the cosign bundle of each, from the same workflow |

A Ladder cloud offers the agent of the latest release to its clusters ("Update" on the Cluster
page) and shows the install command that reads both files from there:

```bash
curl -fsSL https://github.com/k8-ladder/ladder-agent/releases/latest/download/install.sh |
  LADDER_URL=https://<your Ladder> LADDER_INSTALL_URL=https://github.com/k8-ladder/ladder-agent/releases/latest/download sh
```

The agent updates only to an image this workflow on `main` signed (`internal/imagesig`), and runs it
by digest. To check a release yourself:

```bash
cosign verify ghcr.io/k8-ladder/ladder-agent:<version> \
  --certificate-identity https://github.com/k8-ladder/ladder-agent/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-blob install.sh --bundle install.sh.sigstore.json \
  --certificate-identity https://github.com/k8-ladder/ladder-agent/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Layout

| Path | What |
|---|---|
| `cmd/agent` | the agent |
| `cmd/fakebe` | a stand-in backend for development; `GET /starter` renders a minimal starter repository from `cmd/fakebe/starter` |
| `protocol` | wire types, imported by the cloud |
| `install` | the templates of `install.sh` and `agent.yaml`, rendered for a release or by a cloud |
| `cmd/installfiles` | renders the release's `install.sh` and `agent.yaml` |
| `internal/link` | WebSocket client: reconnect, request dispatch, events |
| `internal/cluster` | cluster facts |
| `internal/platform` | the platform from the agent's image: Argo CD's values, the projects, the Applications of Argo CD, Traefik, cert-manager and Metrics Server, the ApplicationSet of apps, the deployer apps sync as and the policies for apps' namespaces; Argo CD's sslip.io host |
| `internal/argo` | Application watcher |
| `internal/inspect` | `kube.read`: objects, events and logs for the assistant |
| `internal/setup` | setup engine, steps, state |
| `internal/helmx` | Helm SDK wrapper |
| `deploy/` | dev manifests for k3d, pointing at fakebe (the real install is `install.sh`) |

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
| `LADDER_REMOTE_UPGRADE` | on | `off` refuses `agent.upgrade` from the cloud |
| `LADDER_AGENT_SIGNATURE` | `off` for an image in no registry, else `keyless` | `keyless` runs only images the release workflow signed, by digest |
| `LADDER_AGENT_SIGNER`, `LADDER_AGENT_SIGNER_ISSUER` | `release.yml` of `k8-ladder/ladder-agent` on `main`, GitHub Actions | the signer's identity, a regular expression, and its OIDC issuer |
| `LADDER_KUBE_READ`, `LADDER_APP_LOGS` | on | `off` refuses the assistant's reads, pod logs |
| `LADDER_READ_NAMESPACES` | all | comma-separated globs the assistant's namespaced reads must match |

## Known gaps in the POC

- The agent only creates Argo CD's repository Secret. Creating the repository and committing the
  starter files (apps and their charts) is the backend's job, before `setup.apply`.
- A Helm release left `pending-install` by a killed pod stops the Argo CD step with a manual hint.
- The watcher picks the Argo CD namespace at startup. Setting up into a non-default namespace needs
  an agent restart to watch it.
- Unit tests cover the sync check of setup, the platform's rendering and move from git, the watcher,
  cluster reads, logs and the upgrade. Applying the platform needs a real cluster (k3d).
