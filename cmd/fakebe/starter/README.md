# GitOps repository

Created by Ladder. Argo CD deploys what is on `main`.

- `apps/<name>/` — one folder per app: `app.yaml` and `values.yaml`.
- `bootstrap/` — Argo CD Applications: `root` (this folder), `argocd` (Argo CD itself), `appsets`.
- `argocd/` — AppProject and ApplicationSet `apps`.
- `platform/argocd/values.yaml` — Helm values of Argo CD. Change the chart version in
  `bootstrap/argocd.yaml` with a pull request to upgrade Argo CD.

Everything here can be edited by hand. Ladder keeps comments and unknown keys.
