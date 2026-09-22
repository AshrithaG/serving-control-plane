package fairness

import (
	"testing"
	"time"
)

// A tenant with twice the weight should receive about twice the service when
// both are backlogged, and the check is on cost served rather than on the
// number of requests, because unequal request sizes are the whole point.
func TestWeightedShare(t *testing.T) {
	d := NewDRR(100)
	d.SetWeight("a", 2)
	d.SetWeight("b", 1)
	for i := 0; i < 200; i++ {
		d.Push(Item{Tenant: "a", Cost: 100})
		d.Push(Item{Tenant: "b", Cost: 100})
	}
	served := map[string]float64{}
	for i := 0; i < 150; i++ {
		it, ok := d.Pop()
		if !ok {
			t.Fatalf("pop %d: scheduler empty with %d queued", i, d.Len())
		}
		served[it.Tenant] += it.Cost
	}
	ratio := served["a"] / served["b"]
	if ratio < 1.8 || ratio > 2.2 {
		t.Fatalf("weighted share a/b = %.2f, want about 2 (a=%v b=%v)", ratio, served["a"], served["b"])
	}
}

// A tenant whose requests are individually more expensive than one quantum
// must still make progress rather than starving forever.
func TestExpensiveItemStillProgresses(t *testing.T) {
	d := NewDRR(10)
	d.SetWeight("big", 1)
	d.Push(Item{Tenant: "big", Cost: 1000})
	if _, ok := d.Pop(); !ok {
		t.Fatal("expensive item never scheduled")
	}
}

// An idle tenant must not bank credit while it sends nothing and then use it to
// jump the queue when it comes back.
func TestIdleTenantDoesNotBankCredit(t *testing.T) {
	d := NewDRR(50)
	d.SetWeight("steady", 1)
	d.SetWeight("bursty", 1)
	for i := 0; i < 20; i++ {
		d.Push(Item{Tenant: "steady", Cost: 50})
	}
	for i := 0; i < 10; i++ {
		d.Pop() // bursty is silent throughout
	}
	d.Push(Item{Tenant: "bursty", Cost: 50})
	served := 0
	for i := 0; i < 4; i++ {
		if it, ok := d.Pop(); ok && it.Tenant == "bursty" {
			served++
		}
	}
	// Two active tenants and four pops: two each is exactly the fair share, so
	// anything above that means the idle tenant banked credit.
	if served > 2 {
		t.Fatalf("bursty tenant served %d times in 4 pops, so it banked credit while idle", served)
	}
}

// Under deadline ordering a request due sooner overtakes one due later from the
// same tenant, and ties keep arrival order.
func TestDeadlineOrderWithinATenant(t *testing.T) {
	d := NewDRR(1000)
	d.OrderByDeadline(true)
	now := time.Now()
	d.Push(Item{Tenant: "a", Cost: 10, Deadline: now.Add(20 * time.Second), Value: "batch"})
	d.Push(Item{Tenant: "a", Cost: 10, Deadline: now.Add(3 * time.Second), Value: "interactive-1"})
	d.Push(Item{Tenant: "a", Cost: 10, Deadline: now.Add(3 * time.Second), Value: "interactive-2"})
	var got []string
	for i := 0; i < 3; i++ {
		it, _ := d.Pop()
		got = append(got, it.Value.(string))
	}
	want := []string{"interactive-1", "interactive-2", "batch"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

// Deadline order must not starve: once the batch request's deadline is the
// nearest, it is served before interactive work that arrived after it.
func TestDeadlineOrderDoesNotStarveBatch(t *testing.T) {
	d := NewDRR(1000)
	d.OrderByDeadline(true)
	now := time.Now()
	d.Push(Item{Tenant: "a", Cost: 10, Deadline: now.Add(2 * time.Second), Value: "batch"})
	d.Push(Item{Tenant: "a", Cost: 10, Deadline: now.Add(3 * time.Second), Value: "interactive"})
	if it, _ := d.Pop(); it.Value.(string) != "batch" {
		t.Fatal("a batch request due first was overtaken by later interactive work")
	}
}

func TestCostAheadCountsOnlyEarlierDeadlines(t *testing.T) {
	d := NewDRR(1000)
	d.OrderByDeadline(true)
	now := time.Now()
	d.Push(Item{Tenant: "a", Cost: 400, Deadline: now.Add(20 * time.Second)})
	d.Push(Item{Tenant: "b", Cost: 70, Deadline: now.Add(2 * time.Second)})
	if got := d.CostAhead(now.Add(3 * time.Second)); got != 70 {
		t.Fatalf("work ahead of a 3s request = %v, want 70 (the 20s batch item is behind it)", got)
	}
}
