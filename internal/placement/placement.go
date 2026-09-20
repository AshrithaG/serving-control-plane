// Package placement chooses which replica serves a request.
//
// Two forces pull against each other. Sending a request to the replica that
// already holds its prefix makes prefill nearly free. Sending it to the least
// loaded replica keeps tail latency down. A policy that only does the first
// starves a hot replica; a policy that only does the second throws the cache
// away. The prefix policy here prefers affinity and abandons it once the
// imbalance passes a threshold, and that threshold is the knob worth measuring.
package placement

import (
	"context"
	"hash/fnv"
	"sync/atomic"

	"github.com/AshrithaG/serving-control-plane/internal/backend"
	"github.com/AshrithaG/serving-control-plane/internal/types"
)

type Policy interface {
	Pick(ctx context.Context, pool *backend.Pool, req types.Request) *backend.Replica
	Name() string
}

// RoundRobin ignores both load and locality. It is the baseline that shows
// what the other two policies are worth.
type RoundRobin struct{ n atomic.Uint64 }

func (p *RoundRobin) Name() string { return "round-robin" }
func (p *RoundRobin) Pick(_ context.Context, pool *backend.Pool, _ types.Request) *backend.Replica {
	i := p.n.Add(1) - 1
	return pool.Replicas[int(i%uint64(len(pool.Replicas)))]
}

// LeastLoaded picks the replica with the fewest requests in flight from the
// router's own view, which is cheaper and fresher than asking the engine.
type LeastLoaded struct{}

func (p *LeastLoaded) Name() string { return "least-loaded" }
func (p *LeastLoaded) Pick(_ context.Context, pool *backend.Pool, _ types.Request) *backend.Replica {
	best := pool.Replicas[0]
	for _, r := range pool.Replicas[1:] {
		if r.Inflight() < best.Inflight() {
			best = r
		}
	}
	return best
}

// PrefixAffinity routes identical leading context to the same replica unless
// that replica is more than Imbalance requests busier than the least loaded
// one, in which case locality is given up for latency.
type PrefixAffinity struct {
	Imbalance int
	fallback  LeastLoaded
}

func (p *PrefixAffinity) Name() string { return "prefix-affinity" }

// PrefixKey mirrors the stand-in engine's cache key: the first 64 bytes. A real
// engine keys on token blocks, so this is the assumption to revisit first when
// the measured hit rate on hardware does not match the simulated one.
func PrefixKey(prompt string) uint64 {
	n := 64
	if len(prompt) < n {
		n = len(prompt)
	}
	h := fnv.New64a()
	h.Write([]byte(prompt[:n]))
	return h.Sum64()
}

func (p *PrefixAffinity) Pick(ctx context.Context, pool *backend.Pool, req types.Request) *backend.Replica {
	preferred := pool.Replicas[int(PrefixKey(req.Prompt)%uint64(len(pool.Replicas)))]
	least := p.fallback.Pick(ctx, pool, req)
	if preferred.Inflight()-least.Inflight() > p.Imbalance {
		return least
	}
	return preferred
}

func ByName(name string, imbalance int) Policy {
	switch name {
	case "round-robin":
		return &RoundRobin{}
	case "prefix-affinity":
		return &PrefixAffinity{Imbalance: imbalance}
	default:
		return &LeastLoaded{}
	}
}
