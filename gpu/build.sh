#!/usr/bin/env bash
# Cross-compile for the GPU box so it needs no Go toolchain, only vLLM.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p gpu/bin
for c in router loadgen; do
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "gpu/bin/$c" "./cmd/$c"
done
echo "built: $(ls gpu/bin | tr '\n' ' ')"
