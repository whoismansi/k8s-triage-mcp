#!/usr/bin/env bash
# Sets up and inspects the demo scenarios on a local kind cluster.
#
#   scripts/demo.sh up              create the cluster, load images, apply all fixtures
#   scripts/demo.sh wait            block until every scenario has reached its failure state
#   scripts/demo.sh show <scenario> print the tool output for one scenario
#   scripts/demo.sh down            delete the cluster
#
# Scenarios: bad-command memory-hog liveness-kill bad-image too-big orders leaky
set -euo pipefail

cluster="k8s-triage-demo"
context="kind-${cluster}"
namespace="triage-demo"
root="$(cd "$(dirname "$0")/.." && pwd)"
binary="${root}/bin/k8s-triage-mcp"
images=(busybox:1.36 nginx:1.27 nginx:1.27-alpine)

kube() { kubectl --context "${context}" -n "${namespace}" "$@"; }

up() {
  if ! kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
    kind create cluster --name "${cluster}" --wait 120s
  fi
  # preloading avoids depending on registry rate limits during a demo.
  for image in "${images[@]}"; do
    docker pull -q "${image}" >/dev/null
    kind load docker-image "${image}" --name "${cluster}" >/dev/null
  done
  kubectl --context "${context}" create namespace "${namespace}" --dry-run=client -o yaml \
    | kubectl --context "${context}" apply -f - >/dev/null

  for fixture in crashloop-bad-command oomkill-low-limit liveness-kill image-pull-error pending-unschedulable leaky-logs; do
    kubectl --context "${context}" apply -f "${root}/fixtures/${fixture}.yaml" >/dev/null
  done

  # the regression scenario needs a healthy first revision before the
  # breaking one, otherwise there is no "before" to diff against.
  kubectl --context "${context}" apply -f "${root}/fixtures/regression-between-revisions/v1-healthy.yaml" >/dev/null
  kube rollout status deploy/orders --timeout=120s >/dev/null
  kubectl --context "${context}" apply -f "${root}/fixtures/regression-between-revisions/v2-broken.yaml" >/dev/null
  echo "fixtures applied to ${context}; run 'scripts/demo.sh wait' next"
}

# pod_state prints the first container's state reason for an app label.
pod_state() {
  kube get pods -l "app=$1" -o jsonpath='{range .items[*]}{.status.containerStatuses[0].state.waiting.reason}{.status.containerStatuses[0].lastState.terminated.reason}{.status.conditions[?(@.type=="PodScheduled")].reason}{" "}{end}' 2>/dev/null
}

wait_for() {
  local app="$1" pattern="$2" timeout="${3:-180}"
  local elapsed=0
  until pod_state "${app}" | grep -Eq "${pattern}"; do
    if (( elapsed >= timeout )); then
      echo "timed out waiting for ${app} to match ${pattern}" >&2
      return 1
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  echo "ready: ${app}"
}

wait_all() {
  wait_for bad-command 'CrashLoopBackOff'
  wait_for memory-hog 'OOMKilled'
  wait_for liveness-kill 'CrashLoopBackOff|Error'
  wait_for bad-image 'ImagePullBackOff|ErrImagePull'
  wait_for too-big 'Unschedulable'
  wait_for leaky 'CrashLoopBackOff'
  wait_for orders 'CreateContainerConfigError'
  # bad-image uses a 30 second progress deadline; wait for it to trip so the
  # deadline finding is visible.
  local elapsed=0
  until kube get deploy bad-image -o jsonpath='{.status.conditions[?(@.type=="Progressing")].reason}' | grep -q ProgressDeadlineExceeded; do
    (( elapsed >= 120 )) && { echo "timed out waiting for bad-image deadline" >&2; return 1; }
    sleep 3; elapsed=$((elapsed + 3))
  done
  echo "all scenarios are in their failure state"
}

first_pod() {
  kube get pods -l "app=$1" -o jsonpath='{.items[0].metadata.name}'
}

# failing_pod picks the orders pod from the new revision, which is the broken one.
failing_pod() {
  kube get pods -l "app=$1" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[0].state.waiting.reason}{"\n"}{end}' \
    | awk '$2 != "" {print $1; exit}'
}

show() {
  local scenario="${1:?usage: scripts/demo.sh show <scenario>}"
  local args=(--context "${context}" -n "${namespace}")
  case "${scenario}" in
    bad-command|memory-hog|liveness-kill|leaky)
      "${binary}" crashloop "${args[@]}" "$(first_pod "${scenario}")" ;;
    bad-image)
      "${binary}" status "${args[@]}" bad-image
      "${binary}" events "${args[@]}" -l app=bad-image ;;
    too-big)
      "${binary}" events "${args[@]}" -l app=too-big
      "${binary}" crashloop "${args[@]}" "$(first_pod too-big)" ;;
    orders)
      "${binary}" status "${args[@]}" orders
      "${binary}" diff "${args[@]}" orders
      "${binary}" crashloop "${args[@]}" "$(failing_pod orders)" ;;
    *)
      echo "unknown scenario: ${scenario}" >&2
      echo "scenarios: bad-command memory-hog liveness-kill bad-image too-big orders leaky" >&2
      return 2 ;;
  esac
}

down() {
  kind delete cluster --name "${cluster}"
}

case "${1:-}" in
  up) up ;;
  wait) wait_all ;;
  show) shift; show "$@" ;;
  down) down ;;
  *) echo "usage: scripts/demo.sh up|wait|show <scenario>|down" >&2; exit 2 ;;
esac
