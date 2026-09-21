# serving-control-plane

An SLO-aware router in front of LLM inference engines: it decides whether a
request can still meet its deadline, shares the GPU between tenants by weight,
and picks the replica most likely to hold the request's prefix.

The question it exists to answer is narrow. vLLM schedules inside one engine.
Nothing above it reasons about deadlines, tenants, or which replica already
holds your context. Is a control plane that does worth building, and what does
it actually buy?

**State.** Measured on real hardware: two vLLM 0.28.0 engines serving
Qwen3-1.7B on one RTX 4090, three seeds, 60 seconds of open-loop traffic per
configuration, 19,353 requests with zero errors. The GPU result disagrees with
the simulator in the way that matters, and both are kept below: the simulated
section is the record of how the policies were developed, the GPU section is
what they actually do.

## What it does

Four configurations, identical in everything except policy, so a difference
between them cannot come from a different client, backend, or workload.

| mode | queueing | admission | placement |
|---|---|---|---|
| `direct` | none | none | one replica, the no-router baseline |
| `rr` | none | none | round robin |
| `fifo` | one central queue, bounded dispatch | none | least loaded |
| `full` | per-tenant deficit round robin | deadline-based | prefix affinity with a load guard |

- **Admission control** projects wait plus service time from token debt the pool
  still owes and the decode rate the router has measured, and refuses at arrival
  what cannot finish inside the request's deadline. A refusal is a typed 429
  with the arithmetic in the body, not a timeout.
- **Fairness** is deficit round robin over tenant queues, charging estimated
  token cost rather than request count, so a tenant sending long requests cannot
  take more than its weighted share.
- **Placement** prefers the replica that already holds the prefix and gives that
  up once it is more than `-imbalance` requests busier than the least loaded one.
- **Goodput** is the metric, not throughput: requests that met their deadline,
  divided by the window over which load was offered.

## Results on an RTX 4090

Two vLLM engines (`--gpu-memory-utilization 0.42`, `--max-num-seqs 16`, prefix
caching on) sharing one RTX 4090, the same workload generator and analysis as
the simulated runs, 70% interactive requests (64 tokens, 3s deadline) and 30%
batch (384 tokens, 20s deadline), `ignore_eos` so every request costs what it
asked for. Median of three seeds, range in brackets. Raw records and the
per-seed aggregate are in `results/gpu-20260921-1533/`.

Offered 16 requests/s, the highest rate run:

| policy | met SLO | goodput/s | TTFT p50 | TTFT p95 | TBT p50 | shed |
|---|---|---|---|---|---|---|
| direct, one engine | 919 [879-939] | 15.29 | 16 ms | 326 ms | 5.2 ms | 0 |
| rr, two engines | 919 [879-940] | 15.29 | 32 ms | 677 ms | 11.1 ms | 0 |
| fifo, two engines | 919 [879-939] | 15.29 | 37 ms | 449 ms | 11.1 ms | 0 |
| full, two engines | 912 [870-919] | 15.18 | 36 ms | 344 ms | 11.1 ms | 9 [7-20] |

At 4 and 8 requests/s every policy met the deadline for every request it was
offered, and the only differences were latency: 4.9 ms between tokens on one
engine against 10.5 ms on two, 14 ms to first token against 28 to 34 ms.

**What the GPU says that the simulator did not.**

- **The GPU was never overloaded.** At every rate tested, goodput tracks the
  offered load. The simulator modelled 8 ms per token per sequence with four
  slots, which made 14 requests/s roughly twice capacity; vLLM's continuous
  batching on a 4090 is far faster than that model, so the overload regime where
  the simulated policies diverged was never reached. The rates were chosen from
  the simulator's capacity, and that was the mistake.
- **Two engines on one GPU made latency worse and bought nothing.** They contend
  for the same SMs, so each engine's decode steps wait on the other's. Time
  between tokens roughly doubled (4.9 to 10.5 ms at 4 requests/s) and median
  time to first token doubled, with no goodput gain at any rate tested. Whether
  the second engine starts paying for itself once one engine saturates is the
  open question the next run is for.
- **Admission control is a small pure cost when nothing is overloaded.** The full
  policy had the best tail latency of the two-engine configurations, p95 time to
  first token 344 ms against 449 for FIFO and 677 for round robin, but on every
  seed it refused 7 to 20 requests that FIFO then served inside their deadline,
  0.7 to 2% of goodput. That is the expected cost of a conservative estimator,
  now measured instead of assumed.
- **The two engines were not equal.** The second engine sized its KV cache from
  the memory the first left free and got 59,776 tokens against 49,904. Placement
  that treats replicas as interchangeable is already wrong on one GPU.
- **Prefix-cache hits were not measured on this path.** vLLM reports them as an
  engine-wide counter, not per request, so the analysis prints n/a rather than
  the zero it printed at first.

## Results, simulated backend

From the stand-in engine, kept as the record of how the policies were built.
Where these disagree with the GPU section above, the GPU section is right: in
particular, the overload regime below never occurred on the real card.

Two engines, four concurrent sequences each, 20s of open-loop Poisson arrivals,
seed 1, mixed interactive (64 output tokens, 3s deadline) and batch (384 tokens,
20s deadline) traffic, 70% of requests reusing one of eight shared prompts.

Offered 14 requests/s, about twice what the pool can serve:

| policy | met SLO | goodput/s | TTFT p50 | TTFT p95 | shed | drained in |
|---|---|---|---|---|---|---|
| direct | 30 | 1.49 | 41.0s | 87.1s | 0 | 115.3s |
| rr | 65 | 3.22 | 16.2s | 32.8s | 0 | 42.5s |
| fifo | 78 | 3.86 | 8.5s | 19.8s | 181 | 42.5s |
| full | 79 | 3.92 | 5.5s | 17.4s | 196 | 38.1s |

Offered 4 requests/s, inside capacity:

| policy | met SLO | goodput/s | TTFT p50 | shed |
|---|---|---|---|---|
| direct | 47 | 2.36 | 6.0s | 0 |
| rr | 81 | 4.07 | 21ms | 0 |
| fifo | 82 | 4.12 | 257ms | 0 |
| full | 77 | 3.87 | 21ms | 5 |

**What this says.** Against plain FIFO with bounded dispatch, the full policy is
a wash on goodput: 79 against 78 at overload, and slightly behind inside
capacity because it sheds five requests it did not need to. What it buys is time
to first token, 5.5s against 8.5s at the median under overload, and a queue that
drains sooner. Against round robin and against a single engine it wins on
everything.

The uncomfortable part is worth stating plainly: most of the goodput a control
plane delivers here comes from bounding dispatch and dropping expired work, both
of which FIFO already does. Deadline admission mostly changes *when* a request
learns it will not be served, from a timeout after the GPU has spent work on it
to a 429 at arrival. That is a real operational property and a modest goodput
one.

## Three bugs the measurements found

Kept because the fixes are the interesting part.

1. **The scheduler ignored weights.** Deficit round robin credited a queue once
   per served item instead of once per visit, so every tenant got one request per
   round whatever its weight, and the weighted-share test read 1.00 where it
   should read 2.00. A separate cap on sweep count starved any request costing
   more than 64 quanta.
2. **Goodput was measured in a way that rewarded shedding.** Dividing successes
   by the span until the last request drained gives a shedding policy a smaller
   denominator for the same numerator. Under that denominator the full policy
   looked 41% ahead of FIFO at 14 requests/s. Divided by the offered-load
   window instead, which is identical across policies, it was 14% behind
   (3.42 against 3.97 goodput/s). The 2% edge it has now appeared only after
   the admission fix below; the saved runs for each stage are in `results/v1-*`
   and `results/v2-*`.
3. **Admission mixed per-replica and pool-level state.** It charged one
   replica's token debt plus the whole router's shared queue against that single
   replica's slots, overestimating the wait by roughly the replica count and
   shedding work the pool could have served.

## Workload identity: SPIFFE and SPIRE

Encryption between the router and its backends is the easy part. The claim
worth testing is authorization: a backend should accept the router and nothing
else, including a pod elsewhere in the cluster holding a perfectly valid
identity of its own.

`deploy/spire/` runs SPIRE 1.15.3 in a kind cluster. The SPIRE agent attests each
pod by its Kubernetes service account and issues an X.509 SVID. Backends serve
mutual TLS and authorize by SPIFFE ID (`spiffe://example.org/ns/scp/sa/router`
only); the router, in turn, only connects to a peer presenting the backend ID.
Certificates live two minutes on purpose, so rotation happens several times
inside one run instead of being assumed. `deploy/spire/verify.sh` produces every
number below; the raw output is in `results/identity/`.

**Rotation under load.** Five minutes of traffic, 861 requests, 0 failed, while
the router went through 6 certificates. Each backend logged all 6 distinct
router certificate serials, so the rotated certificates were actually presented
in new handshakes, not just fetched.

**Refusal, with a positive control.** The same probe binary, run under two
service accounts against a backend:

| attempt | as `intruder` | as `router` |
|---|---|---|
| plain HTTP to the TLS port | refused, 400 | refused, 400 |
| TLS with no client certificate | refused, `certificate required` | refused, `certificate required` |
| mTLS with the pod's own valid SVID | refused, `bad certificate` | **accepted, 200** |

The last row is the one that matters. The intruder's certificate is valid,
issued by the same SPIRE server for the same trust domain; it is refused because
of who it names. The router column is the control: without it, three refusals
could just mean the probe is broken.

**Revocation is not instant, and the number says how slow it is.** Deleting the
router's registration entry mid-traffic, the first request failed **146 seconds**
later, which is longer than the two-minute certificate lifetime. The router's log
shows one more certificate issued around the time of the deletion, before the
agent learned of it, and TLS checks a certificate only at the handshake, so an
already-open connection can outlive the certificate that opened it. Both points
are inferences from this one run; the verification script now stamps wall-clock
times on the deletion and the first failure so the next run can separate them.
The practical reading: revocation in this setup is bounded by the SVID lifetime
plus agent sync plus connection lifetime, and anyone relying on it should cap all
three. Restoring the entry brought traffic back 10 seconds later.

```bash
./deploy/spire/up.sh       # kind cluster, SPIRE, registration entries, router and backends
./deploy/spire/verify.sh   # rotation, refusal, revocation
```

## What the backend is

`cmd/fakebackend` is a model of an engine, not an engine. It reproduces the four
behaviours the router reasons about: prefill proportional to prompt length,
decode slowing as concurrency rises, a bounded number of concurrent sequences,
and a prefix cache that makes repeated context cheap. It exists so the
scheduling logic is already correct when it reaches hardware.

It cannot tell you what vLLM will do. Continuous batching, preemption, chunked
prefill, and real KV block reuse are all absent. Treat every number above as a
statement about the policies, not about GPU serving.

## Limits

- One process, two simulated replicas, one machine. No multi-GPU, no tensor or
  pipeline parallelism, no real KV cache.
- The prefix key is the first 64 bytes of the prompt. A real engine keys on
  token blocks, so the simulated hit rate is optimistic and is the first
  assumption to re-check on hardware.
- The cost estimator learns decode and prefill rates by EWMA from completions.
  Estimator error is a failure mode, not a solved problem: a mis-estimate sheds
  work that would have fit.
- The simulated numbers are single runs, seed 1. The GPU numbers are three
  seeds with ranges, one day, one card.

## Running it

```bash
go test ./...
GO=go DURATION=20s ./run_experiment.sh sweep      # all policies at 4, 8, 14 req/s
GO=go DURATION=20s ./run_experiment.sh fairness   # one tenant floods the other
python3 analysis/analyze.py results/*.jsonl
```

Point it at real engines by replacing the backend URLs:

```bash
go run ./cmd/router -mode full \
  -backends "r0=http://gpu0:8000,r1=http://gpu1:8000" \
  -weights "acme=2,globex=1" -records run.jsonl
```

## Next

1. Rerun on the 4090 at rates that saturate one engine, which the first GPU
   run never reached: `RATES="32 48 64" ./gpu/run_on_4090.sh`. That is where the
   second engine, the placement policy and admission control can actually
   differ, or be shown not to.
2. Read vLLM's engine-wide prefix-cache counters before and after each run, so
   prefix-aware placement can be judged on hardware.
3. Work out whether deadline admission earns its complexity over FIFO plus
   expiry-on-dispatch at overload, and if it does not, say so here.
