# serving-control-plane

An SLO-aware router in front of LLM inference engines: it decides whether a
request can still meet its deadline, shares the GPU between tenants by weight,
and picks the replica most likely to hold the request's prefix.

The question it exists to answer is narrow. vLLM schedules inside one engine.
Nothing above it reasons about deadlines, tenants, or which replica already
holds your context. Is a control plane that does worth building, and what does
it actually buy?

**State.** Measured on real hardware: two vLLM 0.28.0 engines serving
Qwen3-1.7B on one RTX 4090, six offered rates from well below to four times past
saturation, three seeds each, 118,722 requests with zero errors. The simulated
results are kept further down as the record of how the policies were developed;
where they disagree with the GPU, the GPU is right.

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

Two vLLM 0.28.0 engines (`--gpu-memory-utilization 0.42`, `--max-num-seqs 16`,
prefix caching on) serving Qwen3-1.7B on one RTX 4090. The workload generator
and analysis are the same as for the simulated runs: 70% interactive requests
(64 tokens, 3s deadline) and 30% batch requests (384 tokens, 20s deadline),
with `ignore_eos` so every request costs what it asked for. Two runs, six
offered rates, three seeds each, 60 seconds per configuration: 118,722 requests
and zero errors. Raw records are in `results/gpu-20260921-1533/` (4 to 16
requests/s) and `results/gpu-20260921-1736/` (32 to 64).

### Goodput across the whole range

Median of three seeds, in requests per second that met their deadline.

| offered | direct, one engine | rr, two engines | fifo, two engines | full, two engines |
|---|---|---|---|---|
| 4 | 4.07 | 4.07 | 4.07 | 4.07 |
| 8 | 7.67 | 7.67 | 7.67 | 7.67 |
| 16 | 15.29 | 15.29 | 15.29 | 15.18 |
| 32 | 6.48 | 5.20 | **9.65** | 8.28 |
| 48 | 4.28 | 3.72 | 5.13 | **7.15** |
| 64 | 3.63 | 3.30 | 4.25 | **7.35** |

Median time to first token at 64 requests/s: 66.4s direct, 72.3s round robin,
19.3s FIFO, 3.8s full.

**Below the knee, the policies do not matter.** Up to 16 requests/s everything
met its deadline under every policy and goodput equals offered load. The one
cost of admission control there: at 16 requests/s it refused 7 to 20 requests
per seed that FIFO then served in time, 0.7 to 2% of goodput.

**Just past the knee, FIFO wins.** At 32 requests/s FIFO delivers 9.65 against
the full policy's 8.28. Admission control is too conservative there: it refuses
work the engines could have finished.

**Deep past it, only admission control holds up.** From 32 to 64 requests/s the
full policy's goodput stays between 7.15 and 8.28, while FIFO falls from 9.65 to
4.25. Without a queue in the router at all, direct and round robin collapse the
classic way: every request is eventually served, but 79 to 84% of them late at
32 requests/s and 94 to 95% at 64, with median time to first token past a
minute.

### Where the advantage actually comes from

Summed over three seeds at 64 requests/s:

| policy | class | offered | met deadline | shed | finished late |
|---|---|---|---|---|---|
| fifo | interactive | 7,729 | 204 (2.6%) | 7,479 | 46 |
| fifo | batch | 3,262 | 556 (17.0%) | 1,566 | 1,140 |
| full | interactive | 7,729 | 125 (1.6%) | 7,604 | 0 |
| full | batch | 3,260 | 1,199 (36.8%) | 1,710 | 351 |

The headline number needs this table beside it:

- **The full policy's advantage is batch traffic.** FIFO dispatches batch
  requests that are already too old to finish in time: 1,140 of them used GPU
  time and missed their deadline anyway. The full policy refuses those at
  arrival, so the batch requests it admits finish.
- **Neither policy keeps interactive users served past saturation.** A
  3-second deadline is lost to queueing under both, and the full policy
  serves *fewer* interactive requests than FIFO, not more.
- **What admission control admits, it delivers.** No interactive request the
  full policy admitted finished late, at 32 or 64 requests/s. The estimator is
  accurate on what it lets in; the problem is what it keeps out.
- **The missing piece is class priority.** Deficit round robin shares capacity
  by tenant, and nothing puts a 3-second interactive request ahead of a
  20-second batch one. That is the design change this run argues for.

### Other things the GPU showed

- **The second engine never paid for itself on one GPU.** Two engines contend
  for the same SMs. Below the knee they doubled time between tokens (4.9 to
  10.5 ms) for no goodput gain, and past it round robin across two engines stays
  below a single engine at every rate. The policy comparisons above are all
  two-engine; whether the full policy does better still on one engine is untested.
- **The engines were not equal.** The second sized its KV cache from the memory
  the first left free: 59,776 tokens against 49,904.
- **vLLM merges tokens under load.** Counting streamed chunks undercounted
  2.4% of requests in the unqueued configurations at 32 to 64 requests/s, by 1
  to 23 tokens, median 2. Goodput and time to first token do not depend on the count; time
  between tokens for those requests is slightly inflated. The client now reads
  the exact count from vLLM's final usage chunk.
- **Prefix-cache hits were not measured on this path.** vLLM reports them as an
  engine-wide counter, not per request, so the analysis prints n/a.

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

1. Class priority: put interactive requests ahead of batch requests inside each
   tenant's share, then rerun 32 to 64 requests/s and check whether interactive
   goodput recovers without giving back the batch gain.
2. Make admission less conservative near the knee, where FIFO still wins at
   32 requests/s, and measure what that costs deeper past it.
3. Run the full policy on a single engine, since the second engine never paid
   for itself on one GPU.
4. Read vLLM's engine-wide prefix-cache counters before and after each run, so
   prefix-aware placement can be judged on hardware.
