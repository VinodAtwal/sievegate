#!/usr/bin/env bash
#
# End-to-end k8s demo: deploy two mock services + the quality-test proxy, pump
# idempotent traffic through it, fetch the Markdown report, then tear down
# every k8s resource that was created.
#
# Options:
#   -k, --keep        keep the k8s namespace/resources after the run
#   -b, --build       force a rebuild of the images (skipped if already present)
#   -s <secs>         how long to let traffic accumulate (default 30)
#   -r <file>         where to write the report (default ./report.md)
#
# The namespace is ALWAYS deleted on exit unless --keep is given, and the
# cluster is only touched inside the "migration-test" namespace.
#
# Network flakiness: all curls use --max-time/--retry, base images are pulled
# with retries on first build, and an existing image is reused when present.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$DIR")"

NAMESPACE="${NAMESPACE:-migration-test}"
PROXY_IMAGE="${PROXY_IMAGE:-apimigrate/proxy:latest}"
MOCK_IMAGE="${MOCK_IMAGE:-apimigrate/mock:latest}"
TRAFFIC_SECS="${TRAFFIC_SECS:-30}"
REPORT_OUT="${REPORT_OUT:-$DIR/report.md}"
KEEP=0
BUILD=0

usage() {
  echo "usage: $0 [-k|--keep] [-b|--build] [-s seconds] [-r report.md]" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    -k|--keep) KEEP=1; shift ;;
    -b|--build) BUILD=1; shift ;;
    -s) TRAFFIC_SECS="$2"; shift 2 ;;
    -r) REPORT_OUT="$2"; shift 2 ;;
    *) usage ;;
  esac
done

# pull_with_retry <image> — Docker Hub can be flaky; retry the pull.
pull_with_retry() {
  local image="$1"
  for i in 1 2 3 4 5; do
    if docker pull "$image" >/dev/null 2>&1; then
      echo "    pulled $image"
      return 0
    fi
    echo "    pulling $image failed (attempt $i), retrying"
    sleep 3
  done
  echo "ERROR: could not pull $image" >&2
  return 1
}

# build_if_needed <image> <pkg> <base-deps...> — reuse existing image unless
# BUILD=1; otherwise prepare base images and build.
build_if_needed() {
  local image="$1" pkg="$2"
  shift 2
  if [[ "$BUILD" != "1" ]] && docker image inspect "$image" >/dev/null 2>&1; then
    echo "    image $image already present; skipping build (use -b to force)"
    return 0
  fi
  for base in "$@"; do pull_with_retry "$base"; done
  docker build --pull=false --build-arg "PKG=$pkg" -t "$image" "$ROOT"
}

PF_PID=""
cleanup() {
  if [[ -n "$PF_PID" ]]; then kill "$PF_PID" 2>/dev/null || true; fi
  if [[ "$KEEP" == "1" ]]; then
    echo "==> --keep set; leaving resources in namespace '$NAMESPACE'"
    return
  fi
  echo "==> cleaning up k8s namespace '$NAMESPACE'"
  kubectl delete namespace "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

CURL="curl --max-time 5 --retry 3 --retry-delay 1"

echo "==> [1/6] ensuring proxy image ($PROXY_IMAGE)"
build_if_needed "$PROXY_IMAGE" "." golang:1.26-alpine alpine:3.20

echo "==> [2/6] ensuring mock service image ($MOCK_IMAGE)"
build_if_needed "$MOCK_IMAGE" "./example/mockservice" golang:1.26-alpine alpine:3.20

echo "==> [3/6] deploying to namespace '$NAMESPACE'"
kubectl apply -f "$DIR/k8s/namespace.yaml"   # namespace + configmap
kubectl apply -f "$DIR/k8s/mock.yaml"
kubectl apply -f "$DIR/k8s/proxy.yaml"
kubectl apply -f "$DIR/k8s/traffic.yaml"

echo "==> [4/6] waiting for rollouts"
kubectl -n "$NAMESPACE" rollout status deploy/mock-original --timeout=180s
kubectl -n "$NAMESPACE" rollout status deploy/mock-migrated --timeout=180s
kubectl -n "$NAMESPACE" rollout status deploy/migration-proxy --timeout=180s
kubectl -n "$NAMESPACE" rollout status deploy/traffic-generator --timeout=180s

echo "==> [5/6] letting traffic accumulate for ${TRAFFIC_SECS}s"
sleep "$TRAFFIC_SECS"

echo "==> [6/6] fetching markdown report -> $REPORT_OUT"
kubectl -n "$NAMESPACE" port-forward svc/migration-proxy 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
for i in $(seq 1 20); do
  if $CURL -s -o /dev/null http://127.0.0.1:18080/report; then break; fi
  sleep 0.5
done
$CURL -s http://127.0.0.1:18080/report -o "$REPORT_OUT"
kill "$PF_PID" 2>/dev/null || true
PF_PID=""

echo
echo "=============== REPORT PREVIEW (head) ==============="
head -45 "$REPORT_OUT"
echo
echo "full report: $REPORT_OUT"