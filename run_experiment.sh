#!/usr/bin/env bash
# Every policy sees the same seeded workload against freshly started backends,
# so the policy is the only thing that differs between runs.
#
# Scenarios:
#   sweep    the four policies at several offered rates, below and above capacity
#   fairness one tenant floods while the other keeps a steady trickle
set -euo pipefail
cd "$(dirname "$0")"
GO=${GO:-go}
SCENARIO=${1:-sweep}
DURATION=${DURATION:-25s}
SEED=${SEED:-1}
OUT=${OUT:-results}
mkdir -p "$OUT"

$GO build -o /tmp/scp-router ./cmd/router
$GO build -o /tmp/scp-backend ./cmd/fakebackend
$GO build -o /tmp/scp-loadgen ./cmd/loadgen

run_one() { # mode rate tag tenants
  local mode=$1 rate=$2 tag=$3 tenants=$4
  /tmp/scp-backend -addr 127.0.0.1:8100 -max-running 4 >/dev/null 2>&1 & local B0=$!
  /tmp/scp-backend -addr 127.0.0.1:8101 -max-running 4 >/dev/null 2>&1 & local B1=$!
  sleep 0.4
  /tmp/scp-router -addr 127.0.0.1:8080 -mode "$mode" \
    -backends "r0=http://127.0.0.1:8100,r1=http://127.0.0.1:8101" \
    -weights "acme=2,globex=1" -max-inflight 8 \
    -records "$OUT/$tag.jsonl" >/dev/null 2>&1 & local R=$!
  sleep 0.4
  /tmp/scp-loadgen -target http://127.0.0.1:8080/generate -rate "$rate" \
    -duration "$DURATION" -seed "$SEED" -tenants "$tenants"
  sleep 0.5
  kill $R $B0 $B1 2>/dev/null || true
  wait $R $B0 $B1 2>/dev/null || true
}

case "$SCENARIO" in
sweep)
  for RATE in 4 8 14; do
    for MODE in direct rr fifo full; do
      echo "=== $MODE at ${RATE}/s ==="
      run_one "$MODE" "$RATE" "sweep-${RATE}-${MODE}" "acme=1,globex=1"
    done
    echo; echo "--- offered ${RATE}/s ---"
    python3 analysis/analyze.py "$OUT"/sweep-${RATE}-*.jsonl | tee "$OUT/summary-rate-$RATE.txt"
    echo
  done
  ;;
fairness)
  # acme has weight 2 but offers a quarter of the load; globex floods. A
  # weighted scheduler should still hold acme's share up under that pressure.
  for MODE in rr fifo full; do
    echo "=== $MODE, globex flooding ==="
    run_one "$MODE" 14 "fair-${MODE}" "acme=1,globex=4"
  done
  python3 analysis/analyze.py "$OUT"/fair-*.jsonl | tee "$OUT/summary-fairness.txt"
  ;;
*)
  echo "unknown scenario: $SCENARIO" >&2; exit 2;;
esac
