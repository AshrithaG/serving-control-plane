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

# Preflight. On 2026-09-11 this box had a driver/library mismatch after an
# upgrade without a reboot; nothing below works in that state, so stop early.
if ! nvidia-smi >/dev/null 2>&1; then
  echo "nvidia-smi fails on this machine (driver/library mismatch?). The box needs a reboot by its admin." >&2
  nvidia-smi 2>&1 | head -3 >&2
  exit 1
fi

# vLLM lives in a virtualenv, not on the system Python. Find the first one that
# can import it with a working GPU, the same way int8-linear's runner does.
PY=${PYTHON:-}
if [[ -z "$PY" ]]; then
  for cand in "$HOME"/*/.venv/bin/python "$HOME"/*-env/bin/python "$HOME"/.venv/bin/python; do
    [[ -x "$cand" ]] || continue
    if "$cand" -c 'import torch, vllm; assert torch.cuda.is_available()' >/dev/null 2>&1; then
      PY=$cand; break
    fi
  done
fi
if [[ -z "$PY" ]]; then
  echo "no Python with vllm and a working GPU found; set PYTHON=/path/to/venv/bin/python" >&2
  exit 1
fi
VLLM="$(dirname "$PY")/vllm"
echo "using $PY"

for port in 8000 8001 8080; do
  if ss -ltn 2>/dev/null | grep -q ":$port "; then
    echo "port $port is already in use on this machine; stop whatever holds it first" >&2
    exit 1
  fi
done

MODEL=${MODEL:-Qwen/Qwen3-1.7B}
MAX_SEQS=${MAX_SEQS:-16}
MEM=${MEM:-0.42}          # per engine; two engines must fit on one card
# fcfs matches the earlier runs. "priority" is needed for the prio mode, and is
# identical to fcfs for requests that carry no priority, so the other modes in
# the same run stay comparable with each other.
POLICY=${POLICY:-fcfs}
RATES=${RATES:-"4 8 16"}
MODES=${MODES:-"direct rr fifo full edf"}
DURATION=${DURATION:-60s}
SEEDS=${SEEDS:-"1 2 3"}
OUT=${OUT:-results/gpu-$(date +%Y%m%d-%H%M)}
mkdir -p "$OUT"

nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader | tee "$OUT/gpu.txt"
echo "scheduling policy: $POLICY, modes: $MODES, rates: $RATES" | tee -a "$OUT/gpu.txt"
"$PY" -c "import vllm; print('vllm', vllm.__version__)" | tee -a "$OUT/gpu.txt"

start_engine() { # port
  "$VLLM" serve "$MODEL" --port "$1" --gpu-memory-utilization "$MEM" \
    --max-num-seqs "$MAX_SEQS" --enable-prefix-caching --scheduling-policy "$POLICY" \
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

# One at a time: the second engine sizes its KV cache from the memory the first
# one leaves free, so starting both at once races for the same memory.
E0=$(start_engine 8000); wait_ready 8000
E1=$(start_engine 8001); wait_ready 8001
echo "both engines up; about $(( $(echo $SEEDS | wc -w) * $(echo $RATES | wc -w) * $(echo $MODES | wc -w) * 70 / 60 )) minutes to go"
trap 'kill $E0 $E1 2>/dev/null || true' EXIT

for SEED in $SEEDS; do
  for RATE in $RATES; do
    for MODE in $MODES; do
      TAG="gpu-s${SEED}-r${RATE}-${MODE}"
      # prio needs requests waiting inside vLLM for its priority to reorder, so
      # the router lets twice the engines' running slots through; every other
      # mode keeps the queue in the router.
      INFLIGHT=$((2 * MAX_SEQS))
      if [ "$MODE" = prio ]; then INFLIGHT=$((4 * MAX_SEQS)); fi
      echo "=== $TAG ==="
      ./gpu/bin/router -addr 127.0.0.1:8080 -mode "$MODE" -protocol vllm \
        -model "$MODEL" -max-num-seqs "$MAX_SEQS" \
        -backends "r0=http://127.0.0.1:8000,r1=http://127.0.0.1:8001" \
        -weights "acme=2,globex=1" -max-inflight "$INFLIGHT" \
        -records "$OUT/$TAG.jsonl" > "$OUT/$TAG.router.log" 2>&1 &
      R=$!
      sleep 1
      ./gpu/bin/loadgen -target http://127.0.0.1:8080/generate -rate "$RATE" \
        -duration "$DURATION" -seed "$SEED" -tenants "acme=1,globex=1"
      kill $R 2>/dev/null || true; wait $R 2>/dev/null || true
      # Let both engines drain so one run's backlog does not leak into the next.
      sleep 5
    done
    "$PY" analysis/analyze.py "$OUT"/gpu-s${SEED}-r${RATE}-*.jsonl | tee "$OUT/summary-s${SEED}-r${RATE}.txt"
  done
done
echo "results in $OUT"
