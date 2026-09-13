#!/usr/bin/env bash
#
# Local end-to-end demo (no Kubernetes required).
#
# Starts an original + migrated mock service, the quality-test proxy, pumps a
# few dozen idempotent requests through it, then prints the Markdown report.
# All temporary data lives in .demo-tmp/ inside this project and is deleted
# when the script exits.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$DIR"

WORK=".demo-tmp"
mkdir -p "$WORK"
ORIG=$WORK/mock-original
MIG=$WORK/mock-migrated
PROXY=$WORK/proxy
DB=$WORK/migration-demo.db
rm -f "$DB"

pids=()
cleanup() {
  echo
  echo "==> cleaning up ${WORK}/"
  for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "==> building binaries"
go build -o "$PROXY" .
go build -o "$ORIG" ./example/mockservice
cp "$ORIG" "$MIG"

echo "==> starting mock services (original :9001, migrated :9002)"
PORT=9001 MOCK_ROLE=original "$ORIG" >"$WORK/original.log" 2>&1 &
pids+=($!)
PORT=9002 MOCK_ROLE=migrated "$MIG" >"$WORK/migrated.log" 2>&1 &
pids+=($!)

echo "==> starting quality-test proxy on :8080"
sed "s#/tmp/migration-local.db#$DB#" example/config.local.yaml > "$WORK/config.yaml"
"$PROXY" -config "$WORK/config.yaml" >"$WORK/proxy.log" 2>&1 &
pids+=($!)

for i in $(seq 1 20); do
  if curl -s -o /dev/null http://localhost:8080/api/health; then break; fi
  sleep 0.25
done

URL=http://localhost:8080
echo "==> pumping traffic through $URL"
for i in $(seq 1 8); do
  curl -s -o /dev/null "$URL/api/users/1"
  curl -s -o /dev/null "$URL/api/users/2"
  curl -s -o /dev/null "$URL/api/users/missing"
  curl -s -o /dev/null "$URL/api/orders"
  curl -s -o /dev/null "$URL/api/products/42"
  curl -s -o /dev/null "$URL/api/products/7"
done
curl -s -o /dev/null "$URL/api/health"   # denied route -> forwarded only
curl -s -o /dev/null -X POST "$URL/api/orders" # POST -> forwarded only

sleep 1

echo
echo "=============== MARKDOWN REPORT ==============="
curl -s "$URL/report"
echo
echo "=============== JSON REPORT (first 40 lines) ==============="
curl -s "$URL/report.json" | head -40
echo