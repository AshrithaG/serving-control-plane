#!/usr/bin/env bash
# Run on your Mac, on the CMU VPN. Builds the Linux binaries and copies exactly
# what the GPU run needs to the box. Asks for the box's password once.
set -euo pipefail
cd "$(dirname "$0")/.."
HOST=${HOST:-vkenkre@172.24.57.107}
./gpu/build.sh
# A stamp the run script records, so a result can always be traced to the build
# that produced it, and a stale copy on the box is visible rather than silent.
echo "$(git rev-parse --short HEAD)$(git diff --quiet || echo -dirty) built $(date -u +%Y-%m-%dT%H:%MZ)" > gpu/bin/BUILD
rsync -a --relative gpu/bin gpu/run_on_4090.sh analysis/analyze.py "$HOST":serving-control-plane/
echo "copied build $(cat gpu/bin/BUILD) to $HOST:~/serving-control-plane"
