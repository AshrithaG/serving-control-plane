#!/usr/bin/env bash
# Run on your Mac, on the CMU VPN. Builds the Linux binaries and copies exactly
# what the GPU run needs to the box. Asks for the box's password once.
set -euo pipefail
cd "$(dirname "$0")/.."
HOST=${HOST:-vkenkre@172.24.57.107}
./gpu/build.sh
rsync -a --relative gpu/bin gpu/run_on_4090.sh analysis/analyze.py "$HOST":serving-control-plane/
echo "copied to $HOST:~/serving-control-plane"
