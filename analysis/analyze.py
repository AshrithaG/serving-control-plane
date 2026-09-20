#!/usr/bin/env python3
"""Turn per-request records into the numbers the report is allowed to claim.

Every metric here is computed from the router's own JSONL trace. Nothing is
read from a counter printed while the run was in flight, because a counter
cannot be re-checked after the fact and a trace can.
"""
import json, math, sys
from pathlib import Path


def pct(xs, p):
    if not xs:
        return float("nan")
    xs = sorted(xs)
    k = (len(xs) - 1) * p / 100.0
    lo, hi = math.floor(k), math.ceil(k)
    if lo == hi:
        return xs[int(k)]
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def jain(values):
    """Jain's fairness index over per-tenant service: 1.0 is a perfectly equal
    share, 1/n is one tenant taking everything."""
    if not values:
        return float("nan")
    s = sum(values)
    sq = sum(v * v for v in values)
    if sq == 0:
        return float("nan")
    return (s * s) / (len(values) * sq)


def load(path):
    rows = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                rows.append(json.loads(line))
    return rows


def summarize(path, window_s=None):
    rows = load(path)
    done = [r for r in rows if r.get("last_token_ms") and not r.get("shed") and not r.get("error")]
    shed = [r for r in rows if r.get("shed")]
    err = [r for r in rows if r.get("error")]

    ttft = [r["first_token_ms"] - r["arrival_ms"] for r in done]
    e2e = [r["last_token_ms"] - r["arrival_ms"] for r in done]
    tbt = [
        (r["last_token_ms"] - r["first_token_ms"]) / (r["out_tokens"] - 1)
        for r in done
        if r.get("out_tokens", 0) > 1
    ]
    met = [r for r in done if (r["last_token_ms"] - r["arrival_ms"]) <= r["deadline_ms"]]

    # Goodput is divided by the window over which load was OFFERED, not by the
    # span until the last request drained. Dividing by the drain span rewards a
    # policy for shedding: it finishes sooner, so the same number of successes
    # lands in a smaller denominator. The offered window is the same for every
    # policy under comparison, which is the only denominator that lets the
    # numbers be compared at all.
    arrivals = [r["arrival_ms"] for r in rows]
    offered_span = (max(arrivals) - min(arrivals)) / 1000.0 if arrivals else 0.0
    span = window_s if window_s is not None else offered_span
    stamps = arrivals + [r.get("last_token_ms", 0) for r in rows]
    drain_span = (max(stamps) - min(stamps)) / 1000.0 if stamps else 0.0

    by_tenant = {}
    for r in met:
        by_tenant[r["tenant"]] = by_tenant.get(r["tenant"], 0) + r.get("out_tokens", 0)

    reasons = {}
    for r in shed:
        reasons[r.get("shed_reason", "?")] = reasons.get(r.get("shed_reason", "?"), 0) + 1

    return {
        "run": Path(path).stem,
        "policy": rows[0]["policy"] if rows else "?",
        "offered": len(rows),
        "completed": len(done),
        "shed": len(shed),
        "errors": len(err),
        "met_slo": len(met),
        "goodput_rps": len(met) / span if span else float("nan"),
        "offered_rps": len(rows) / offered_span if offered_span else float("nan"),
        "drain_s": drain_span,
        "ttft_p50": pct(ttft, 50), "ttft_p95": pct(ttft, 95), "ttft_p99": pct(ttft, 99),
        "tbt_p50": pct(tbt, 50), "tbt_p95": pct(tbt, 95),
        "e2e_p50": pct(e2e, 50), "e2e_p95": pct(e2e, 95), "e2e_p99": pct(e2e, 99),
        "prefix_hit": sum(1 for r in done if r.get("prefix_hit")) / len(done) if done else float("nan"),
        "tenant_tokens": by_tenant,
        "jain": jain(list(by_tenant.values())),
        "shed_reasons": reasons,
        "window_s": span,
        "tenant_share": {k: v / sum(by_tenant.values()) for k, v in by_tenant.items()} if by_tenant else {},
    }


def main(paths):
    rows = [summarize(p) for p in paths]
    cols = [
        ("policy", "{:<8}"), ("offered", "{:>7}"), ("met_slo", "{:>7}"), ("shed", "{:>5}"),
        ("errors", "{:>6}"), ("goodput_rps", "{:>11.2f}"), ("ttft_p50", "{:>8.0f}"),
        ("ttft_p95", "{:>8.0f}"), ("ttft_p99", "{:>8.0f}"), ("tbt_p50", "{:>7.1f}"),
        ("e2e_p95", "{:>8.0f}"), ("prefix_hit", "{:>10.2f}"), ("jain", "{:>5.3f}"),
    ]
    head = "  ".join(name.rjust(len(fmt.format(0 if ":" in fmt and "f" in fmt else 0).strip()) if False else max(len(name), 5)) for name, fmt in cols)
    print("  ".join(f"{name:>11}" for name, _ in cols))
    for r in rows:
        cells = []
        for name, fmt in cols:
            v = r[name]
            try:
                cells.append(f"{fmt.format(v):>11}")
            except (ValueError, TypeError):
                cells.append(f"{str(v):>11}")
        print("  ".join(cells))
    print()
    for r in rows:
        share = {k: f"{v:.2f}" for k, v in r["tenant_share"].items()}
        print(f"{r['policy']}: offered={r['offered_rps']:.1f}/s over {r['window_s']:.1f}s, "
              f"drained in {r['drain_s']:.1f}s, tenant tokens={r['tenant_tokens']} share={share}, "
              f"shed={r['shed_reasons']}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("usage: analyze.py run1.jsonl [run2.jsonl ...]", file=sys.stderr)
        raise SystemExit(2)
    main(sys.argv[1:])
