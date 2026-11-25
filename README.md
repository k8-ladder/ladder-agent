# Ladder agent

**The open-source agent that connects your Kubernetes cluster to [Ladder](https://k8-ladder.com/).**
It sets up Argo CD and GitOps in your cluster and tells Ladder what runs there. It connects out to
Ladder, so nothing has to reach into your cluster.

[Ladder](https://k8-ladder.com/) deploys your team's apps to your own Kubernetes cluster as simply
as a PaaS does, while everything underneath stays plain GitOps that you own: you fill in a form,
Ladder turns it into a commit in the cluster's git repository, and Argo CD in your cluster deploys
it. The agent is the small program Ladder installs in your cluster for that. Its code is public,
so you can read what runs in your cluster and check that the image you run was built from this
repository.

- **Website and sign-up:** [k8-ladder.com](https://k8-ladder.com/)
- **User guide:** [k8-ladder.com/docs](https://k8-ladder.com/docs)

## How it works

```
           your cluster                                 Ladder
 +-----------------------------+                +--------------------+
 |  ladder-agent               |---- HTTPS ---->|  portal            |
 |                             |   (outbound)   |                    |
 |  Argo CD                    |-- reads git -->|  repository        |
 |     |                       |                +--------------------+
 |     v                       |                  or in your GitHub
 |  your apps                  |
 +-----------------------------+
```

- The agent opens one connection out to Ladder over HTTPS and keeps it open. Ladder never connects
  into the cluster, so clusters behind NAT, behind a firewall or on a laptop work. The cluster needs
  outbound HTTPS only.
- Every change you make in Ladder is a commit in the cluster's repository. Argo CD deploys what is
  on `main`. The agent never writes to git.
- If you remove the agent, Argo CD and your apps keep running from git.

## What the agent does

- **Sets up GitOps.** Installs Argo CD in the namespace `argocd`, gives it read access to the
  cluster's repository and hands it over to git: from then on Argo CD manages itself and your apps
  from the repository. With Traefik turned on in Ladder it adds Traefik and cert-manager for HTTPS,
  and it adds Metrics Server when the cluster has none.
- **Reports what runs.** Kubernetes version, nodes, free CPU and memory, storage classes, and the
  sync and health status of every app, for the cluster page in Ladder.
- **Deploys right after a change.** When `main` gets a commit, the agent asks Argo CD to read git at
  once rather than in a few minutes.
- **Helps find problems.** Events and pod logs of your apps, and read-only lookups for Ladder's
  assistant, as `kubectl get` would show them. It never shows the values of Secrets.
- **Stores app secrets.** Writes the environment secrets you enter in Ladder straight into the
  cluster, once. It cannot read them back, and git holds only a reference to them.
- **Updates itself.** When you press **Update** in Ladder, to a new release signed by this
  repository's release workflow.

## Install

Ladder shows the exact command with your connect code: in Ladder, add a cluster and choose
**Import a cluster**. When Ladder creates the cluster for you (in DigitalOcean, on your Linux
server or as a sandbox on your computer), it installs the agent itself.

You need:

- `kubectl` pointing at the cluster, with `cluster-admin` on it;
- outbound HTTPS from the cluster: to Ladder, and to the registries of the agent's image
  (`ghcr.io`) and of Argo CD;
- no Argo CD in the cluster yet: Ladder installs its own.

Run the command from Ladder in a terminal. It looks like this:

```bash
curl -fsSL https://github.com/k8-ladder/ladder-agent/releases/latest/download/install.sh |
  LADDER_URL=https://<your Ladder> LADDER_INSTALL_URL=https://github.com/k8-ladder/ladder-agent/releases/latest/download sh
```

The script:

1. shows the kubectl context it is about to change and asks you to confirm;
2. checks that you have `cluster-admin`;
3. asks for the connect code from Ladder (valid for an hour, for one cluster);
4. installs the agent: the namespace `ladder`, a ServiceAccount, a Deployment, and a Secret with
   the agent's token;
5. waits until the agent runs. The page in Ladder then moves on to setup by itself.

It prints each command before it runs it, and running it again is safe: on a cluster Ladder already
knows, it gives that cluster's agent a new token instead of adding another cluster.

The script and the agent's image come from this repository's releases, not from the Ladder you
install for: check that the command reads `install.sh` from `github.com/k8-ladder/ladder-agent`.

## What the agent may do in your cluster

| When | Rights |
|---|---|
| During setup | `cluster-admin`, to install Argo CD and the rest of the platform. |
| After setup | The agent drops `cluster-admin` by itself and keeps only what Ladder's actions need (below). |
| Updates that bring a new Argo CD | The cluster page in Ladder shows a command that lends the agent `cluster-admin` again for a few seconds. You run it. |

After setup the agent:

- **reads** the cluster as the built-in ClusterRole `view` does, plus nodes, storage classes,
  custom resource definitions, RBAC objects and Argo CD's objects. No Secrets, no `exec`, `attach`
  or `port-forward` into pods;
- **creates** the namespaces of apps and their environment Secrets, which it cannot read back. An
  admission policy lets it write only `Opaque` Secrets, and none in `kube-*` namespaces;
- **in Argo CD's namespace**, manages Argo CD's Ingress and asks Argo CD to refresh. Another admission
  policy keeps it from changing anything else of Argo CD's;
- **in its own namespace `ladder`**, keeps its state and token, and updates its own Deployment.

The exact rules are in [`internal/platform/files/agent.yaml`](internal/platform/files/agent.yaml).
See them in your cluster with:

```bash
kubectl get clusterrolebinding ladder-agent ladder-agent-view ladder-agent-setup --ignore-not-found
kubectl describe clusterrole ladder-agent
```

## Turn things off

You decide what Ladder may do in your cluster, with settings on the agent's Deployment. Updates keep
them.

```bash
kubectl -n ladder set env deploy/ladder-agent LADDER_REMOTE_UPGRADE=off
```

| Setting | Effect |
|---|---|
| `LADDER_REMOTE_UPGRADE=off` | **Update** in Ladder stops working; you update the agent by hand with `kubectl set image`. |
| `LADDER_KUBE_READ=off` | The assistant cannot read the cluster. |
| `LADDER_APP_LOGS=off` | Ladder shows no pod logs. |
| `LADDER_READ_NAMESPACES=shop-*,billing` | The assistant reads only these namespaces (comma-separated, `*` matches any characters). |

## See what Ladder did

The agent writes down in your cluster every change Ladder asked for and every look the assistant
took: setup, updates of the agent, new secret values (their names, never the values), the Argo CD
sign-in and the assistant's reads. Ladder has no way to erase them:

```bash
kubectl -n ladder get configmap ladder-audit -o jsonpath='{.data.log}'   # the last 500
kubectl -n ladder get events                                            # the last hour
```

## Updates and signatures

When a new release is out, the cluster page in Ladder offers it within about ten minutes, and an
admin presses **Update**. The old agent keeps running until the new one has connected, and if the
new one does not start, the agent goes back to its version.

The agent runs a new version only if this repository's [release workflow](.github/workflows/release.yml)
on `main` signed it, with [cosign](https://docs.sigstore.dev/cosign/overview/) keyless: it checks
the signature before it updates, and runs the image by its digest. So nobody who takes over Ladder,
or pushes an image to the registry, can make your cluster run other code.

Each [release](https://github.com/k8-ladder/ladder-agent/releases) `agent-<version>` has
`install.sh`, `agent.yaml` (the agent's manifest, with the image by digest) and their signatures
(`*.sigstore.json`). To check them yourself:

```bash
cosign verify ghcr.io/k8-ladder/ladder-agent:<version> \
  --certificate-identity https://github.com/k8-ladder/ladder-agent/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify-blob install.sh --bundle install.sh.sigstore.json \
  --certificate-identity https://github.com/k8-ladder/ladder-agent/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Stop or remove the agent

To stop Ladder without stopping your apps, for example if you stop trusting it:

```bash
kubectl -n ladder scale deploy/ladder-agent --replicas=0   # Ladder reaches nothing in the cluster
kubectl -n argocd get secret repo-deploy -o yaml > repo-deploy.yaml
kubectl -n argocd delete secret repo-deploy                # Argo CD reads no new commits
```

Your apps keep running the version they run. To start again, `kubectl apply -f repo-deploy.yaml`
and scale the agent back to 1.

To remove the agent for good, press **Disconnect** on the cluster page in Ladder and run the
commands it shows:

```bash
kubectl delete namespace ladder
kubectl delete clusterrolebinding ladder-agent ladder-agent-view ladder-agent-setup --ignore-not-found
kubectl delete clusterrole ladder-agent --ignore-not-found
kubectl delete validatingadmissionpolicy,validatingadmissionpolicybinding ladder-agent-secrets ladder-agent-refresh --ignore-not-found
kubectl -n argocd delete role,rolebinding ladder-agent --ignore-not-found
```

Argo CD and your apps keep running, and the cluster's repository stays.

## Troubleshooting

| What you see | What to do |
|---|---|
| Ladder keeps "Waiting for the agent…", or says "Agent offline" | See what the agent does: `kubectl -n ladder get pods` and `kubectl -n ladder logs deploy/ladder-agent`. The cluster needs outbound HTTPS to Ladder. Restart it with `kubectl -n ladder rollout restart deploy/ladder-agent`. Your apps keep running meanwhile. |
| The agent's pod is in `ErrImagePull` or `ImagePullBackOff` | The nodes cannot pull the image: check that they reach `ghcr.io`. |
| The log says the token is unknown, or the namespace `ladder` is gone | Run the install command again. The cluster stays the same in Ladder, with its repository and apps. |
| Ladder warns that an agent in another cluster used this cluster's token | The agent's Secret was copied into another cluster, such as by restoring a backup there. Ladder refused that agent; remove it there with the commands the warning shows. |

More in the [user guide](https://k8-ladder.com/docs). Questions: **Support** in Ladder.

## For developers

How to build, test and run the agent, its layout and all its settings are in
[DEVELOPMENT.md](DEVELOPMENT.md). The protocol between the agent and Ladder is in
[PROTOCOL.md](PROTOCOL.md).

## License

Apache License 2.0, in [`LICENSE`](LICENSE).
