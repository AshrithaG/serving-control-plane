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
)

// Item is one queued request with the cost the scheduler should charge for it.
type Item struct {
	Tenant string
	Cost   float64 // estimated service cost in tokens
	Value  any
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
	q.items.PushBack(it)
	d.n++
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
