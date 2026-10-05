#!/bin/sh
# Installs the Ladder agent into the cluster of the current kubectl context
# (spec §5.3):
#
#   curl -fsSL {{.PublicURL}}/install.sh | sh
#
# Environment: LADDER_CODE skips the code prompt, LADDER_YES=1 skips the
# confirmation, LADDER_URL overrides the Ladder address.
set -eu

LADDER_URL="${LADDER_URL:-{{.PublicURL}}}"
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
	fail "you need cluster-admin on $context: the agent gets cluster-admin, and only a cluster admin can grant it"

code="${LADDER_CODE:-}"
[ -n "$code" ] || ask code "Connect code from $LADDER_URL: "
code=$(printf '%s' "$code" | tr -cd 'A-Za-z0-9-')
name=$(printf '%s' "$context" | tr -d '"\\')

show "curl -X POST $LADDER_URL/api/agent/enroll"
resp=$(curl -sS -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' \
	-d "{\"code\":\"$code\",\"clusterName\":\"$name\"}" "$LADDER_URL/api/agent/enroll") ||
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

# An older install may have bound ladder-agent to another role, and a
# binding's roleRef cannot change: replace it.
role=$(kubectl get clusterrolebinding ladder-agent -o jsonpath='{.roleRef.name}' 2>/dev/null || true)
if [ -n "$role" ] && [ "$role" != cluster-admin ]; then
	show "kubectl delete clusterrolebinding ladder-agent  # bound to $role by an older install"
	kubectl delete clusterrolebinding ladder-agent
fi

show "curl -fsSL $LADDER_URL/install/agent.yaml | kubectl apply -f -"
curl -fsSL "$LADDER_URL/install/agent.yaml" | kubectl apply -f -

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
