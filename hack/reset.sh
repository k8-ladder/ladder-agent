#!/usr/bin/env bash
# Undo Ladder setup on a dev cluster: Argo CD, Gitea, setup state. Keeps the
# agent, its Secret and its access, and restarts it.
set -euo pipefail

kubectl -n argocd delete applications --all --wait=false 2>/dev/null || true
for app in $(kubectl -n argocd get applications -o name 2>/dev/null); do
  kubectl -n argocd patch "$app" --type merge -p '{"metadata":{"finalizers":null}}' || true
done
helm -n argocd uninstall argocd --ignore-not-found
kubectl delete namespace argocd ladder-git --ignore-not-found
kubectl get crd -o name | grep argoproj.io | xargs -r kubectl delete
kubectl -n ladder delete configmap ladder-setup-state --ignore-not-found
kubectl -n ladder delete secret ladder-connection ladder-setup-credentials --ignore-not-found
kubectl -n ladder rollout restart deploy/ladder-agent
