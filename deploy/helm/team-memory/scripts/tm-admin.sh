#!/usr/bin/env bash
# Runs one Team Memory operator command (init, scope, member, token, jobs,
# mirror, ...) in a short-lived pod with the operator database URL from the
# release's database Secret. The pod is removed afterwards.
#
#   scripts/tm-admin.sh <namespace> <release-fullname> <command> [args...]
#   scripts/tm-admin.sh team-memory team-memory init --org acme --owner "Ada"
#
# The pod carries the release's labels for the "migrate" component, so the
# chart's NetworkPolicy lets it reach PostgreSQL and nothing else. The
# command's output (including passwords and tokens printed once) is read
# from the pod log and the pod is deleted right after, which also removes
# that log from the node. (kubectl run --attach loses the output of
# commands that finish before it attaches.)
#
# Before the first install (no release yet), name the image and the
# database Secret explicitly:
#   TM_IMAGE=ghcr.io/team-memory/team-memory:0.6.0 TM_DB_SECRET=team-memory-db \
#     scripts/tm-admin.sh team-memory team-memory db create-login --role app --user tm_app
set -euo pipefail
if [[ $# -lt 3 ]]; then
  sed -n '2,17p' "$0"; exit 2
fi
ns=$1 full=$2; shift 2
key=${TM_OPERATOR_KEY:-operator-url}
if deploy=$(kubectl -n "$ns" get deploy "$full-api" -o json 2>/dev/null); then
  image=${TM_IMAGE:-$(jq -r '.spec.template.spec.containers[0].image' <<<"$deploy")}
  secret=${TM_DB_SECRET:-$(jq -r '.spec.template.spec.containers[0].env[] | select(.name=="TEAM_MEMORY_APP_DATABASE_URL") | .valueFrom.secretKeyRef.name' <<<"$deploy")}
  name=$(jq -r '.spec.template.metadata.labels["app.kubernetes.io/name"]' <<<"$deploy")
  instance=$(jq -r '.spec.template.metadata.labels["app.kubernetes.io/instance"]' <<<"$deploy")
else
  : "${TM_IMAGE:?no release $full in $ns yet: set TM_IMAGE and TM_DB_SECRET}" "${TM_DB_SECRET:?set TM_DB_SECRET}"
  image=$TM_IMAGE secret=$TM_DB_SECRET name=team-memory instance=${TM_INSTANCE:-$full}
fi
args=$(jq -cn '$ARGS.positional' --args -- "$@")
overrides=$(jq -cn --arg image "$image" --arg secret "$secret" --arg key "$key" --argjson args "$args" '{
  apiVersion: "v1",
  spec: {
    automountServiceAccountToken: false,
    securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, seccompProfile: {type: "RuntimeDefault"}},
    restartPolicy: "Never",
    containers: [{
      name: "tm-admin", image: $image, args: $args,
      securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: ["ALL"]}},
      env: [{name: "DATABASE_URL", valueFrom: {secretKeyRef: {name: $secret, key: $key}}}]
    }]
  }
}')
pod="tm-admin-$(date +%s)-$RANDOM"
trap 'kubectl -n "$ns" delete pod "$pod" --ignore-not-found --wait=false >/dev/null' EXIT
kubectl -n "$ns" run "$pod" --quiet --restart=Never --image="$image" \
  --labels="app.kubernetes.io/name=$name,app.kubernetes.io/instance=$instance,app.kubernetes.io/component=migrate" \
  --overrides="$overrides" >/dev/null
phase=""
for _ in $(seq 1 300); do
  phase=$(kubectl -n "$ns" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ $phase == Succeeded || $phase == Failed ]] && break
  sleep 1
done
kubectl -n "$ns" logs "$pod"
[[ $phase == Succeeded ]]
