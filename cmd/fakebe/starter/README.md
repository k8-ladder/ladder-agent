# GitOps repository

Created by Ladder. Argo CD deploys what is on `main`.

- `apps/<name>/` — one folder per app: `app.yaml` and `values.yaml`.

Argo CD, its projects and the ApplicationSet of apps are not here: Ladder's agent applies them from
its own image. Everything here can be edited by hand. Ladder keeps comments and unknown keys.
