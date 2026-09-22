// Package router is the control plane: it accepts requests, decides whether
// they can be served in time, shares the GPU between tenants, picks a replica
// and records what happened.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AshrithaG/serving-control-plane/internal/admission"
	"github.com/AshrithaG/serving-control-plane/internal/backend"
	"github.com/AshrithaG/serving-control-plane/internal/fairness"
	"github.com/AshrithaG/serving-control-plane/internal/placement"
	"github.com/AshrithaG/serving-control-plane/internal/record"
	"github.com/AshrithaG/serving-control-plane/internal/types"
)

// Mode selects which of the four configurations under comparison is running.
// They differ only here, so a measured difference cannot come from a different
// client, a different backend or a different workload.
type Mode string

const (
	// Direct sends everything straight to one replica: no queue, no admission,
	// no placement. This is the "no router" baseline.
	Direct Mode = "direct"
	// RoundRobin spreads across replicas and does nothing else.
	RoundRobin Mode = "rr"
	// FIFO queues centrally with bounded dispatch and least-loaded placement,
	// but admits everything.
	FIFO Mode = "fifo"
	// Full is the policy under test: per-tenant deficit round robin, deadline
	// admission control, prefix-aware placement, and shedding.
	Full Mode = "full"
	// EDF is Full with each tenant's queue in deadline order, and admission
	// that charges an arriving request only for queued work due before it.
	// It exists because the 2026-09-21 saturation run showed Full refusing
	// nearly every 3-second request: it charged them for queued batch work
	// they would never actually have waited behind.
	EDF Mode = "edf"
	// Prio is EDF that also hands each request's deadline to vLLM as its
	// priority, so the engine orders its own waiting queue the same way. vLLM
	// 0.28 only preempts a running request when the KV cache is full, so the
	// prediction, written before the GPU run, is that this matches EDF.
	Prio Mode = "prio"
	// Reserve is EDF with batch requests capped at a share of dispatch slots,
	// so a short interactive request is not left waiting for a slot held by a
	// long batch request. That wait is what EDF alone cannot remove.
	Reserve Mode = "reserve"
)

// deadlineOrdered reports whether a mode queues and admits by deadline.
func deadlineOrdered(m Mode) bool { return m == EDF || m == Prio || m == Reserve }

type Config struct {
	Mode        Mode
	Pool        *backend.Pool
	Placement   placement.Policy
	Admission   *admission.Controller
	Quantum     float64
	Weights     map[string]float64
	MaxInflight int
	Records     *record.Writer
	// BatchShare is the fraction of dispatch slots batch requests may hold at
	// once in Reserve mode.
	BatchShare float64
}

type queued struct {
	req     types.Request
	arrival time.Time
	rec     record.Record
	done    chan struct{}
	status  int
}

type Router struct {
	cfg    Config
	drr    *fairness.DRR
	notify chan struct{}
	slots  chan struct{}

	mu            sync.Mutex
	queuedTokens  float64
	admitted      atomic.Int64
	shed          atomic.Int64
	batchInflight atomic.Int64
	completed     atomic.Int64
	failed        atomic.Int64
	expiredInLine atomic.Int64
}

func New(cfg Config) *Router {
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 8
	}
	rt := &Router{
		cfg:    cfg,
		drr:    fairness.NewDRR(cfg.Quantum),
		notify: make(chan struct{}, 1024),
		slots:  make(chan struct{}, cfg.MaxInflight),
	}
	for t, w := range cfg.Weights {
		rt.drr.SetWeight(t, w)
	}
	rt.drr.OrderByDeadline(deadlineOrdered(cfg.Mode))
	return rt
}

func (rt *Router) queued_(delta float64) {
	rt.mu.Lock()
	rt.queuedTokens += delta
	rt.mu.Unlock()
}

// load sums the whole pool: token debt owed by every replica, the router's own
// queue, and the total number of concurrent sequences the pool can run.
func (rt *Router) load(ctx context.Context) admission.Load {
	var committed float64
	slots := 0
	for _, rep := range rt.cfg.Pool.Replicas {
		committed += float64(rep.Outstanding())
		slots += rep.Stats(ctx).MaxRunning
	}
	return admission.Load{CommittedTokens: committed, QueuedTokens: rt.QueuedTokens(), Slots: slots}
}

func (rt *Router) QueuedTokens() float64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.queuedTokens
}

// Handle is the intake path. It is synchronous from the client's point of view:
// the response returns when the last token has been generated, or immediately
// with 429 if the request is shed.
func (rt *Router) Handle(w http.ResponseWriter, r *http.Request) {
	var req types.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	arrival := time.Now()
	rec := record.Record{
		ID: req.ID, Tenant: req.Tenant, Class: string(req.Class),
		Policy: string(rt.cfg.Mode), ArrivalMS: rt.cfg.Records.Since(arrival),
		PromptTokens: req.PromptTokens(), DeadlineMS: req.DeadlineMS,
	}

	switch rt.cfg.Mode {
	case Direct, RoundRobin:
		// The status has to come from the dispatch. Answering 200 regardless
		// hid every backend failure from the client, including a refused mTLS
		// handshake, which is exactly the failure the identity tests look for.
		w.WriteHeader(rt.dispatchNow(r.Context(), req, rec, arrival))
		return
	}

	// Admission is a decision about the system, so it is made against the
	// replica this request would actually land on.
	rep := rt.cfg.Placement.Pick(r.Context(), rt.cfg.Pool, req)
	load := rt.load(r.Context())
	if deadlineOrdered(rt.cfg.Mode) {
		load.QueuedTokens = rt.drr.CostAhead(req.Deadline(arrival))
	}
	d := rt.cfg.Admission.Decide(rep, load, req, 0)
	if !d.Admit {
		rt.shed.Add(1)
		rec.Shed = true
		rec.ShedReason = d.Reason
		rec.AdmitMS = rt.cfg.Records.Since(time.Now())
		_ = rt.cfg.Records.Write(rec)
		w.Header().Set("X-Shed-Reason", d.Reason)
		http.Error(w, fmt.Sprintf("shed: %s (est wait %.0fms, serve %.0fms, deadline %dms)",
			d.Reason, d.EstWaitMS, d.EstServeMS, req.DeadlineMS), http.StatusTooManyRequests)
		return
	}

	rt.admitted.Add(1)
	rec.AdmitMS = rt.cfg.Records.Since(time.Now())
	q := &queued{req: req, arrival: arrival, rec: rec, done: make(chan struct{})}
	cost := float64(req.PromptTokens() + req.MaxTokens)
	rt.queued_(cost)
	tenant := req.Tenant
	if rt.cfg.Mode == FIFO {
		tenant = "all" // one queue: the point of the FIFO baseline
	}
	rt.drr.Push(fairness.Item{Tenant: tenant, Cost: cost, Deadline: req.Deadline(arrival), Value: q})
	select {
	case rt.notify <- struct{}{}:
	default:
	}

	select {
	case <-q.done:
		w.WriteHeader(q.status)
	case <-r.Context().Done():
	}
}

// Run is the scheduler loop. It holds no lock while dispatching, so a slow
// backend cannot block intake.
func (rt *Router) Run(ctx context.Context) {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-rt.notify:
		case <-tick.C:
		}
		for {
			select {
			case rt.slots <- struct{}{}:
			default:
				goto full
			}
			var it fairness.Item
			var ok bool
			if rt.cfg.Mode == Reserve {
				it, ok = rt.drr.PopEligible(rt.withinBatchShare)
			} else {
				it, ok = rt.drr.Pop()
			}
			if !ok {
				<-rt.slots
				break
			}
			q := it.Value.(*queued)
			rt.queued_(-it.Cost)
			batch := q.req.Class == types.Batch
			if batch {
				rt.batchInflight.Add(1)
			}
			go func() {
				defer func() {
					if batch {
						rt.batchInflight.Add(-1)
					}
					<-rt.slots
				}()
				rt.serve(ctx, q)
			}()
		}
	full:
	}
}

// withinBatchShare admits interactive work always and batch work only while
// batch requests hold less than their share of dispatch slots.
func (rt *Router) withinBatchShare(it fairness.Item) bool {
	q := it.Value.(*queued)
	if q.req.Class != types.Batch {
		return true
	}
	share := rt.cfg.BatchShare
	if share <= 0 || share > 1 {
		share = 0.5
	}
	limit := int64(share * float64(rt.cfg.MaxInflight))
	if limit < 1 {
		limit = 1
	}
	return rt.batchInflight.Load() < limit
}

// serve runs one admitted request. A request whose deadline passed while it
// waited is dropped here rather than sent to the GPU: the work can no longer
// be useful, and spending decode slots on it slows the requests that can be.
func (rt *Router) serve(ctx context.Context, q *queued) {
	defer close(q.done)
	now := time.Now()
	if now.After(q.req.Deadline(q.arrival)) {
		rt.expiredInLine.Add(1)
		rt.shed.Add(1)
		q.rec.Shed = true
		q.rec.ShedReason = "deadline passed while queued"
		q.status = http.StatusTooManyRequests
		_ = rt.cfg.Records.Write(q.rec)
		return
	}
	rep := rt.cfg.Placement.Pick(ctx, rt.cfg.Pool, q.req)
	q.rec.Replica = rep.Name
	q.rec.DispatchMS = rt.cfg.Records.Since(now)

	var prio *int64
	if rt.cfg.Mode == Prio {
		// Earlier deadline, lower number, served first by the engine.
		p := q.req.Deadline(q.arrival).UnixMilli()
		prio = &p
	}
	res, err := rep.GenerateWithPriority(ctx, q.req.ID, q.req.Prompt, q.req.MaxTokens, prio)
	if err != nil {
		rt.failed.Add(1)
		q.rec.Error = err.Error()
		q.status = http.StatusBadGateway
		_ = rt.cfg.Records.Write(q.rec)
		return
	}
	rt.completed.Add(1)
	q.rec.FirstTokenMS = rt.cfg.Records.Since(res.FirstToken)
	q.rec.LastTokenMS = rt.cfg.Records.Since(res.LastToken)
	q.rec.OutTokens = res.OutTokens
	q.rec.PrefixHit = res.PrefixHit
	q.status = http.StatusOK
	_ = rt.cfg.Records.Write(q.rec)
}

// dispatchNow is the unqueued path used by the Direct and RoundRobin
// baselines: straight to a replica, no admission, no fairness.
func (rt *Router) dispatchNow(ctx context.Context, req types.Request, rec record.Record, arrival time.Time) int {
	var rep *backend.Replica
	if rt.cfg.Mode == Direct {
		rep = rt.cfg.Pool.Replicas[0]
	} else {
		rep = rt.cfg.Placement.Pick(ctx, rt.cfg.Pool, req)
	}
	rt.admitted.Add(1)
	rec.Replica = rep.Name
	rec.DispatchMS = rt.cfg.Records.Since(time.Now())
	res, err := rep.Generate(ctx, req.ID, req.Prompt, req.MaxTokens)
	if err != nil {
		rt.failed.Add(1)
		rec.Error = err.Error()
		_ = rt.cfg.Records.Write(rec)
		return http.StatusBadGateway
	}
	rt.completed.Add(1)
	rec.FirstTokenMS = rt.cfg.Records.Since(res.FirstToken)
	rec.LastTokenMS = rt.cfg.Records.Since(res.LastToken)
	rec.OutTokens = res.OutTokens
	rec.PrefixHit = res.PrefixHit
	_ = rt.cfg.Records.Write(rec)
	return http.StatusOK
}

// Stats is the operator view, served at /stats.
func (rt *Router) Stats(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"mode":            rt.cfg.Mode,
		"admitted":        rt.admitted.Load(),
		"shed":            rt.shed.Load(),
		"expired_queued":  rt.expiredInLine.Load(),
		"completed":       rt.completed.Load(),
		"failed":          rt.failed.Load(),
		"queued_requests": rt.drr.Len(),
		"queued_tokens":   rt.QueuedTokens(),
		"batch_inflight":  rt.batchInflight.Load(),
	})
}
