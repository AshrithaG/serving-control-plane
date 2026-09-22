// Package fairness schedules across tenants with deficit round robin.
//
// The property worth having is that a tenant sending long requests cannot take
// more than its weighted share of service, without the scheduler having to
// preempt anything. Each tenant accumulates a deficit proportional to its
// weight and can only dequeue work whose estimated cost fits the deficit it has
// accumulated, so cost, not request count, is what gets shared.
package fairness

import (
	"container/list"
	"sync"
	"time"
)

// Item is one queued request with the cost the scheduler should charge for it.
type Item struct {
	Tenant string
	Cost   float64 // estimated service cost in tokens
	// Deadline orders a tenant's queue when deadline ordering is on. Zero
	// means the item keeps arrival order.
	Deadline time.Time
	Value    any
}

type queue struct {
	weight  float64
	deficit float64
	items   *list.List
}

// DRR is a deficit round robin scheduler over named tenant queues.
type DRR struct {
	mu      sync.Mutex
	quantum float64
	order   []string
	queues  map[string]*queue
	next    int
	n       int
	// credited records whether the queue at next has already been given its
	// quantum for this visit. Without it a weighted queue would be credited
	// once per served item and weights would have no effect.
	credited bool
	// byDeadline orders each tenant's queue earliest deadline first instead of
	// by arrival. Tenants still share capacity by weight; within a tenant's
	// share, the request due soonest goes first.
	byDeadline bool
}

// OrderByDeadline switches every tenant queue to earliest-deadline-first.
//
// Deadline order rather than a strict interactive-first rule: strict priority
// starves batch work for as long as interactive work keeps arriving, while
// deadline order puts a 3-second request ahead of a 20-second one and still
// lets the batch request through once its own deadline is the nearest.
func (d *DRR) OrderByDeadline(on bool) {
	d.mu.Lock()
	d.byDeadline = on
	d.mu.Unlock()
}

func NewDRR(quantum float64) *DRR {
	return &DRR{quantum: quantum, queues: map[string]*queue{}}
}

// SetWeight registers a tenant. Weight 2 gets twice the service of weight 1.
func (d *DRR) SetWeight(tenant string, weight float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q, ok := d.queues[tenant]
	if !ok {
		q = &queue{items: list.New()}
		d.queues[tenant] = q
		d.order = append(d.order, tenant)
	}
	q.weight = weight
}

func (d *DRR) Push(it Item) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q, ok := d.queues[it.Tenant]
	if !ok {
		q = &queue{items: list.New(), weight: 1}
		d.queues[it.Tenant] = q
		d.order = append(d.order, it.Tenant)
	}
	if d.byDeadline && !it.Deadline.IsZero() {
		// Insert before the first item due later, so equal deadlines keep
		// arrival order.
		for e := q.items.Front(); e != nil; e = e.Next() {
			other := e.Value.(Item)
			if !other.Deadline.IsZero() && other.Deadline.After(it.Deadline) {
				q.items.InsertBefore(it, e)
				d.n++
				return
			}
		}
	}
	q.items.PushBack(it)
	d.n++
}

// CostAhead is the queued work due before the given deadline, across every
// tenant. Under deadline ordering that is the work an arriving request actually
// waits behind; charging it for everything in the queue, including batch work
// due much later, is what made admission control refuse interactive requests
// that would have finished in time.
func (d *DRR) CostAhead(deadline time.Time) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	total := 0.0
	for _, q := range d.queues {
		for e := q.items.Front(); e != nil; e = e.Next() {
			it := e.Value.(Item)
			if !it.Deadline.IsZero() && it.Deadline.Before(deadline) {
				total += it.Cost
			}
		}
	}
	return total
}

func (d *DRR) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

// Depth reports the queue length for one tenant, which admission control needs
// to estimate how long a new arrival would wait.
func (d *DRR) Depth(tenant string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if q, ok := d.queues[tenant]; ok {
		return q.items.Len()
	}
	return 0
}

// Pop returns the next item, or false when every queue is empty. A single
// sweep that finds nothing is not an empty scheduler: a queue can be non-empty
// but short of deficit, so the loop keeps handing out quanta until something
// fits or every queue is genuinely empty.
func (d *DRR) Pop() (Item, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n == 0 {
		return Item{}, false
	}
	// Deficit round robin proper: on arriving at a queue, credit it once, then
	// serve from it while its deficit lasts. Pop returns one item at a time, so
	// "staying" means not advancing the cursor, and credited records that the
	// quantum for this visit has already been handed out.
	for {
		name := d.order[d.next]
		q := d.queues[name]
		if q.items.Len() == 0 {
			q.deficit = 0 // an idle tenant does not bank credit
			d.credited = false
			d.next = (d.next + 1) % len(d.order)
			continue
		}
		w := q.weight
		if w <= 0 {
			w = 1 // an unweighted tenant still makes progress
		}
		if !d.credited {
			q.deficit += d.quantum * w
			d.credited = true
		}
		front := q.items.Front()
		it := front.Value.(Item)
		if it.Cost <= q.deficit {
			q.items.Remove(front)
			q.deficit -= it.Cost
			d.n--
			return it, true
		}
		// Does not fit this visit: leave the deficit banked and move on. A
		// request larger than one quantum is served after enough rounds.
		d.credited = false
		d.next = (d.next + 1) % len(d.order)
	}
}
