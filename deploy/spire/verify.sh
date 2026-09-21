#!/usr/bin/env bash
# Evidence for the identity claims, in three phases. Every number printed here
# is what the README quotes.
#
#   1. rotation: five minutes of traffic across several 2-minute certificates
#   2. refusal:  an intruder with a valid identity is refused every way it
#                tries, and the same probe as the router gets in, which shows
#                the probe works
#   3. revocation: delete the router's registration and time how long until
#                its traffic fails, then restore it and time the recovery
set -euo pipefail
cd "$(dirname "$0")"
CTX=${CTX:-kind-scp-identity}
K="kubectl --context $CTX"
S="$K -n spire exec spire-server-0 -c spire-server -- /opt/spire/bin/spire-server"
ROUTER_ID=spiffe://example.org/ns/scp/sa/router
OUT=${OUT:-../../results/identity}
mkdir -p "$OUT"
now() { python3 -c 'import time; print(f"{time.time():.1f}")'; }

$K -n scp port-forward svc/router 18080:8080 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -fsS -o /dev/null http://127.0.0.1:18080/stats && break; sleep 1; done
stat() { curl -fsS http://127.0.0.1:18080/stats | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['completed'], d['failed'])"; }

loadgen() { # name seconds
  $K -n scp delete job "$1" --ignore-not-found >/dev/null
  $K -n scp create job "$1" --image=scp:identity -- /usr/local/bin/loadgen \
    -target http://router.scp.svc:8080/generate -rate 3 -duration "${2}s" \
    -tenants acme=1 -interactive-share 1 -interactive-tokens 16 -interactive-slo-ms 10000 >/dev/null
}

echo "== 1. rotation under traffic =="
read c0 f0 < <(stat)
loadgen traffic 300
$K -n scp wait --for=condition=complete job/traffic --timeout=420s >/dev/null
read c1 f1 < <(stat)
echo "requests completed: $((c1 - c0)), failed: $((f1 - f0))" | tee "$OUT/rotation.txt"
$K -n scp logs deploy/router | grep -c "svid router" | sed 's/^/router certificates used: /' | tee -a "$OUT/rotation.txt"
for b in backend-0 backend-1; do
  $K -n scp logs deploy/$b | grep -c "peer $ROUTER_ID" | sed "s/^/distinct router certificates seen by $b: /" | tee -a "$OUT/rotation.txt"
done

echo "== 2. refusal, with a positive control =="
for sa in intruder router; do
  $K -n scp delete pod "probe-$sa" --ignore-not-found >/dev/null
  sed "s/probe-SA/probe-$sa/; s/serviceAccountName: SA/serviceAccountName: $sa/" probe-pod.yaml | $K apply -f - >/dev/null
  $K -n scp wait --for=jsonpath='{.status.phase}'=Succeeded "pod/probe-$sa" --timeout=120s >/dev/null || true
  echo "-- as $sa" | tee -a "$OUT/refusal.txt"
  $K -n scp logs "probe-$sa" | tee -a "$OUT/refusal.txt"
done

echo "== 3. revocation =="
loadgen revoke 420
sleep 20
ENTRY=$($S entry show -spiffeID "$ROUTER_ID" | awk -F': ' '/Entry ID/{print $2; exit}')
t_del=$(now)
$S entry delete -entryID "$ENTRY" >/dev/null
echo "deleted router entry $ENTRY at $(date -u +%H:%M:%S)" | tee "$OUT/revocation.txt"
read cb fb < <(stat)
t_fail=""
for _ in $(seq 1 60); do
  sleep 5
  read c f < <(stat)
  if [ "$f" -gt "$fb" ]; then t_fail=$(now); break; fi
done
if [ -n "$t_fail" ]; then
  echo "first failed request $(python3 -c "print(round($t_fail - $t_del))")s after deletion, at $(date -u +%H:%M:%S)" | tee -a "$OUT/revocation.txt"
else
  echo "no failures within 300s of deletion" | tee -a "$OUT/revocation.txt"
fi
t_add=$(now)
$S entry create -parentID spiffe://example.org/ns/spire/sa/spire-agent -spiffeID "$ROUTER_ID" \
  -selector k8s:ns:scp -selector k8s:sa:router >/dev/null
read ca fa < <(stat)
t_ok=""
for _ in $(seq 1 60); do
  sleep 5
  read c f < <(stat)
  if [ "$c" -gt "$ca" ] && [ "$f" -eq "$fa" ]; then t_ok=$(now); break; fi
  ca=$c; fa=$f
done
if [ -n "$t_ok" ]; then
  echo "traffic succeeding again $(python3 -c "print(round($t_ok - $t_add))")s after the entry was restored" | tee -a "$OUT/revocation.txt"
else
  echo "no recovery within 300s of restoring the entry" | tee -a "$OUT/revocation.txt"
fi
$K -n scp delete job revoke --ignore-not-found >/dev/null
