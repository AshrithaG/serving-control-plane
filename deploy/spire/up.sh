#!/usr/bin/env bash
# Stand up the identity demo from nothing: a kind cluster, SPIRE, registration
# entries, and the router with two backends speaking mTLS.
set -euo pipefail
cd "$(dirname "$0")"
CLUSTER=${CLUSTER:-scp-identity}

kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
CTX="kind-$CLUSTER"
docker build -q -t scp:identity ../.. >/dev/null
kind load docker-image scp:identity --name "$CLUSTER" >/dev/null

kubectl --context "$CTX" apply -f spire.yaml >/dev/null
kubectl --context "$CTX" -n spire rollout status statefulset/spire-server --timeout=180s
kubectl --context "$CTX" -n spire rollout status daemonset/spire-agent --timeout=180s

S="kubectl --context $CTX -n spire exec spire-server-0 -c spire-server -- /opt/spire/bin/spire-server"
$S entry show -spiffeID spiffe://example.org/ns/spire/sa/spire-agent | grep -q "Entry ID" || \
  $S entry create -node -spiffeID spiffe://example.org/ns/spire/sa/spire-agent \
    -selector k8s_psat:cluster:"$CLUSTER" -selector k8s_psat:agent_ns:spire -selector k8s_psat:agent_sa:spire-agent >/dev/null
for sa in router backend intruder; do
  $S entry show -spiffeID "spiffe://example.org/ns/scp/sa/$sa" | grep -q "Entry ID" || \
    $S entry create -parentID spiffe://example.org/ns/spire/sa/spire-agent \
      -spiffeID "spiffe://example.org/ns/scp/sa/$sa" -selector k8s:ns:scp -selector "k8s:sa:$sa" >/dev/null
done
echo "registered: agent node entry plus router, backend, intruder"

kubectl --context "$CTX" apply -f workloads.yaml >/dev/null
for d in backend-0 backend-1 router; do
  kubectl --context "$CTX" -n scp rollout status "deployment/$d" --timeout=180s
done
