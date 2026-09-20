// Package admission decides whether a request can still meet its deadline, and
// refuses it now if it cannot.
//
// The argument for refusing early: a request admitted into a queue it cannot
// clear in time consumes GPU work and then misses anyway, and it slows every
// request already running. Goodput, not throughput, is what the caller wants,
// so the honest answer to "we cannot serve this in time" is a typed rejection
// at arrival rather than a timeout later.
package admission

import (
	"time"

	"github.com/AshrithaG/serving-control-plane/internal/backend"
	"github.com/AshrithaG/serving-control-plane/internal/types"
)

type Mode string

const (
	Off      Mode = "off"      // admit everything, the baseline
	Deadline Mode = "deadline" // refuse what cannot finish in time
)

// Decision is what the controller concluded and why, so a rejected request can
// be explained without re-deriving the arithmetic.
type Decision struct {
	Admit       bool
	Reason      string
	EstWaitMS   float64
	EstServeMS  float64
	DeadlineMS  int
	Concurrency int
}

type Controller struct {
	Mode Mode
	// Safety scales the estimate before comparing it to the deadline. Above 1
	// it sheds earlier and wastes capacity; below 1 it admits work that misses.
	// The right value is an empirical question, which is why it is a flag.
	Safety float64
}

// Load is the pool-level state an admission decision needs. It is pool-level
// on purpose: the router's queue is shared by every replica, so charging it
// against one replica's slots overestimates the wait by roughly the replica
// count, and that overestimate sheds work the GPU could have served.
type Load struct {
	CommittedTokens float64 // token debt already owed across every replica
	QueuedTokens    float64 // token work waiting in the router's own queues
	Slots           int     // concurrent sequences the pool can run
}

// Estimate models the pool as Slots parallel slots draining committed token
// work at the decode rate the router has measured on the chosen replica. It is
// deliberately simple: the router cannot see inside the engine's scheduler, and
// an estimate that pretends otherwise would be a fiction with more decimals.
func Estimate(r *backend.Replica, load Load, req types.Request) (waitMS, serveMS float64) {
	prefillUS, decodeMS, _ := r.Estimate()
	slots := load.Slots
	if slots <= 0 {
		slots = 1
	}
	wait := (load.CommittedTokens + load.QueuedTokens) * decodeMS / float64(slots)
	serve := float64(req.PromptTokens())*prefillUS/1000.0 + float64(req.MaxTokens)*decodeMS
	return wait, serve
}

func (c *Controller) Decide(r *backend.Replica, load Load, req types.Request, waited time.Duration) Decision {
	wait, serve := Estimate(r, load, req)
	d := Decision{
		EstWaitMS: wait, EstServeMS: serve, DeadlineMS: req.DeadlineMS,
		Concurrency: int(load.CommittedTokens / float64(max(req.MaxTokens, 1))),
	}
	if c.Mode == Off {
		d.Admit = true
		d.Reason = "admission off"
		return d
	}
	safety := c.Safety
	if safety <= 0 {
		safety = 1.0
	}
	budget := float64(req.DeadlineMS) - float64(waited.Milliseconds())
	if (wait+serve)*safety <= budget {
		d.Admit = true
		d.Reason = "fits deadline"
		return d
	}
	d.Admit = false
	d.Reason = "projected completion past deadline"
	return d
}
