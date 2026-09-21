#!/usr/bin/env bash
# The measurement the README's simulated numbers are waiting for: the same four
# policies, the same workload generator and the same analysis, against two real
# vLLM engines sharing one RTX 4090.
#
# Run on the GPU box from the repo root after copying gpu/bin and analysis/:
#   ./gpu/run_on_4090.sh
#
# Two engines on one GPU is a deliberate choice, not a stand-in for two GPUs:
# they contend for the same SMs and memory bandwidth, so placement decisions
# have real consequences, and the report must say it was one GPU.
set -euo pipefail
cd "$(dirname "$0")/.."

MODEL=${MODEL:-Qwen/Qwen3-1.7B}
MAX_SEQS=${MAX_SEQS:-16}
MEM=${MEM:-0.42}          # per engine; two engines must fit on one card
RATES=${RATES:-"4 8 16"}
DURATION=${DURATION:-60s}
SEEDS=${SEEDS:-"1 2 3"}
OUT=${OUT:-results/gpu-$(date +%Y%m%d-%H%M)}
mkdir -p "$OUT"

nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader | tee "$OUT/gpu.txt"
python3 -c "import vllm; print('vllm', vllm.__version__)" | tee -a "$OUT/gpu.txt"

start_engine() { # port
  vllm serve "$MODEL" --port "$1" --gpu-memory-utilization "$MEM" \
    --max-num-seqs "$MAX_SEQS" --enable-prefix-caching --disable-log-requests \
    > "$OUT/engine-$1.log" 2>&1 &
  echo $!
}
wait_ready() { # port
  for _ in $(seq 1 300); do
    curl -fsS "http://127.0.0.1:$1/health" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "engine on $1 never became healthy; see $OUT/engine-$1.log" >&2
  return 1
}

E0=$(start_engine 8000); wait_ready 8000
E1=$(start_engine 8001); wait_ready 8001
trap 'kill $E0 $E1 2>/dev/null || true' EXIT

for SEED in $SEEDS; do
  for RATE in $RATES; do
    for MODE in direct rr fifo full; do
      TAG="gpu-s${SEED}-r${RATE}-${MODE}"
      echo "=== $TAG ==="
      ./gpu/bin/router -addr 127.0.0.1:8080 -mode "$MODE" -protocol vllm \
        -model "$MODEL" -max-num-seqs "$MAX_SEQS" \
        -backends "r0=http://127.0.0.1:8000,r1=http://127.0.0.1:8001" \
        -weights "acme=2,globex=1" -max-inflight $((2 * MAX_SEQS)) \
        -records "$OUT/$TAG.jsonl" > "$OUT/$TAG.router.log" 2>&1 &
      R=$!
      sleep 1
      ./gpu/bin/loadgen -target http://127.0.0.1:8080/generate -rate "$RATE" \
        -duration "$DURATION" -seed "$SEED" -tenants "acme=1,globex=1"
      kill $R 2>/dev/null || true; wait $R 2>/dev/null || true
      # Let both engines drain so one run's backlog does not leak into the next.
      sleep 5
    done
    python3 analysis/analyze.py "$OUT"/gpu-s${SEED}-r${RATE}-*.jsonl | tee "$OUT/summary-s${SEED}-r${RATE}.txt"
  done
done
echo "results in $OUT"
