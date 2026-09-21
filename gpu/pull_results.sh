#!/usr/bin/env bash
# Run on your Mac after the GPU run finishes. Copies the results back.
set -euo pipefail
cd "$(dirname "$0")/.."
HOST=${HOST:-vkenkre@172.24.57.107}
rsync -a "$HOST":serving-control-plane/results/ results/
ls -d results/gpu-* | tail -1
