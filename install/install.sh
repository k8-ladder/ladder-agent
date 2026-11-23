#!/bin/sh
# Installs the Ladder agent into the cluster of the current kubectl context
# (spec §5.3):
#
#   curl -fsSL {{if .PublicURL}}{{.PublicURL}}/install.sh | sh{{else}}<release>/install.sh | LADDER_URL=<Ladder> LADDER_INSTALL_URL=<release> sh{{end}}
#
# Environment: LADDER_CODE skips the code prompt, LADDER_YES=1 skips the
# confirmation, LADDER_URL overrides the Ladder address. LADDER_INSTALL_URL
# is where the agent's manifest is read from instead of Ladder: a release of
# this script and the manifest, with the agent's image by digest, that does
# not come from the cloud you install for. The script of such a release has
# no address of its own and needs LADDER_URL:
#
#   curl -fsSL <release>/install.sh | LADDER_URL=<Ladder> LADDER_INSTALL_URL=<release> sh
#
# LADDER_INGRESS_PORT
# is the port people reach the cluster's ingress on, when not 80, such as
# 8080 for k3d -p 8080:80@loadbalancer. sandbox.sh sets LADDER_CLUSTER, the
# cluster in Ladder this agent is for.
#
# Ladder knows a cluster by the UID of its kube-system namespace: run again
# where its agent ran, the script gives that cluster a new agent token and
# reinstalls the agent, rather than adding another cluster.
set -eu

LADDER_URL="${LADDER_URL:-{{.PublicURL}}}"
INSTALL_URL="${LADDER_INSTALL_URL:-$LADDER_URL/install}"
NAMESPACE=ladder

say() { printf '%s\n' "$*"; }
fail() { printf 'ladder: %s\n' "$*" >&2; exit 1; }
show() { printf '+ %s\n' "$*"; }

# Under "curl | sh" stdin is the script itself, so prompts read the terminal.
ask() {
	{ : </dev/tty; } 2>/dev/null || fail "no terminal to ask on; set LADDER_CODE and LADDER_YES=1"
	printf '%s' "$2" >/dev/tty
	IFS= read -r "$1" </dev/tty
}

# json_field NAME: the string value of "NAME" in the JSON on stdin.
json_field() { sed -n "s/.*\"$1\" *: *\"\([^\"]*\)\".*/\1/p"; }

[ -n "$LADDER_URL" ] || fail "set LADDER_URL to the address of your Ladder: curl -fsSL <release>/install.sh | LADDER_URL=https://<Ladder> sh"
command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v kubectl >/dev/null 2>&1 || fail "kubectl is required"

context=$(kubectl config current-context 2>/dev/null) || fail "kubectl has no current context"
server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
say "Kubernetes context: $context ($server)"
if [ -z "${LADDER_YES:-}" ]; then
	ask answer "Install the Ladder agent into $context? [y/N] "
	case "$answer" in
	y | Y | yes | YES) ;;
	*) fail "cancelled" ;;
	esac
fi

show "kubectl auth can-i '*' '*' --all-namespaces"
[ "$(kubectl auth can-i '*' '*' --all-namespaces 2>/dev/null)" = yes ] ||
	fail "you need cluster-admin on $context: the agent gets it for setup, and only a cluster admin can grant it"

code="${LADDER_CODE:-}"
[ -n "$code" ] || ask code "Connect code from $LADDER_URL: "
code=$(printf '%s' "$code" | tr -cd 'A-Za-z0-9-')
name=$(printf '%s' "$context" | tr -d '"\\')
cluster=$(printf '%s' "${LADDER_CLUSTER:-}" | tr -cd 'a-z0-9_')
port=$(printf '%s' "${LADDER_INGRESS_PORT:-}" | tr -cd '0-9')
show "kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'"
uid=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}' 2>/dev/null | tr -cd 'A-Za-z0-9-' || true)

show "curl -X POST $LADDER_URL/api/agent/enroll"
resp=$(curl -sS -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' \
	-d "{\"code\":\"$code\",\"clusterName\":\"$name\",\"clusterID\":\"$cluster\",\"kubeUID\":\"$uid\",\"ingressPort\":\"$port\"}" "$LADDER_URL/api/agent/enroll") ||
	fail "cannot reach $LADDER_URL"
status=$(printf '%s\n' "$resp" | tail -n 1)
body=$(printf '%s\n' "$resp" | sed '$d')
if [ "$status" != 200 ]; then
	msg=$(printf '%s' "$body" | json_field message)
	fail "${msg:-enroll failed with HTTP $status}"
fi
token=$(printf '%s' "$body" | json_field token)
url=$(printf '%s' "$body" | json_field url)
[ -n "$token" ] && [ -n "$url" ] || fail "unexpected answer from $LADDER_URL"
if printf '%s' "$body" | grep -q '"reconnected": *true'; then
	say "Ladder already knows this cluster as $(printf '%s' "$body" | json_field clusterName): its agent gets a new token."
fi

show "curl -fsSL $INSTALL_URL/agent.yaml | kubectl apply -f -"
curl -fsSL "$INSTALL_URL/agent.yaml" | kubectl apply -f -

# The token goes through a pipe, so it is neither on disk nor in the process list.
show "kubectl apply -f -  # Secret $NAMESPACE/ladder-agent with the agent token"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: ladder-agent\n  namespace: %s\nstringData:\n  token: "%s"\n  url: "%s"\n' \
	"$NAMESPACE" "$token" "$url" | kubectl apply -f -

# Restart so a running agent picks up a new token.
show "kubectl -n $NAMESPACE rollout restart deploy/ladder-agent"
kubectl -n "$NAMESPACE" rollout restart deploy/ladder-agent
show "kubectl -n $NAMESPACE rollout status deploy/ladder-agent --timeout=2m"
kubectl -n "$NAMESPACE" rollout status deploy/ladder-agent --timeout=2m ||
	fail "the agent did not start; see: kubectl -n $NAMESPACE describe pod -l app.kubernetes.io/name=ladder-agent"

say ""
say "The Ladder agent is running. Follow the setup at $LADDER_URL"
