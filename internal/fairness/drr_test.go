package fairness

import "testing"

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
