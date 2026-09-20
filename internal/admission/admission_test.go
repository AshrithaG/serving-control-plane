package admission

import (
	"testing"
	"time"

	"github.com/AshrithaG/serving-control-plane/internal/backend"
	"github.com/AshrithaG/serving-control-plane/internal/types"
)

func req(maxTokens, deadlineMS int) types.Request {
	return types.Request{Prompt: "four", MaxTokens: maxTokens, DeadlineMS: deadlineMS}
}

func TestOffAdmitsEverything(t *testing.T) {
	c := &Controller{Mode: Off}
	r := backend.NewReplica("r0", "http://x")
	d := c.Decide(r, Load{CommittedTokens: 1e9, Slots: 1}, req(1000, 1), 0)
	if !d.Admit {
		t.Fatal("admission off must admit regardless of the estimate")
	}
}

func TestShedsWhenDeadlineCannotBeMet(t *testing.T) {
	c := &Controller{Mode: Deadline, Safety: 1}
	r := backend.NewReplica("r0", "http://x") // seeded at 10ms per token
	// 500 output tokens is about 5s of decode against a 100ms deadline.
	d := c.Decide(r, Load{Slots: 4}, req(500, 100), 0)
	if d.Admit {
		t.Fatalf("admitted a request that cannot finish: wait=%.0f serve=%.0f budget=100", d.EstWaitMS, d.EstServeMS)
	}
}

func TestAdmitsWhenItFits(t *testing.T) {
	c := &Controller{Mode: Deadline, Safety: 1}
	r := backend.NewReplica("r0", "http://x")
	d := c.Decide(r, Load{Slots: 4}, req(10, 60000), 0)
	if !d.Admit {
		t.Fatalf("refused a request with a minute of headroom: %+v", d)
	}
}

// Time already spent queueing has to count against the deadline, or a request
// that waited most of its budget still looks admissible.
func TestTimeAlreadyWaitedCountsAgainstTheBudget(t *testing.T) {
	c := &Controller{Mode: Deadline, Safety: 1}
	r := backend.NewReplica("r0", "http://x")
	fresh := c.Decide(r, Load{Slots: 4}, req(20, 1000), 0)
	stale := c.Decide(r, Load{Slots: 4}, req(20, 1000), 950*time.Millisecond)
	if !fresh.Admit {
		t.Fatal("fresh request should fit a 1s deadline")
	}
	if stale.Admit {
		t.Fatal("a request with 50ms of budget left was admitted for 200ms of work")
	}
}

// The router's queue is shared by the pool, so a second replica must halve the
// projected wait rather than leaving it unchanged.
func TestPoolCapacityShortensTheWait(t *testing.T) {
	r := backend.NewReplica("r0", "http://x")
	one, _ := Estimate(r, Load{QueuedTokens: 4000, Slots: 4}, req(10, 1000))
	two, _ := Estimate(r, Load{QueuedTokens: 4000, Slots: 8}, req(10, 1000))
	if two >= one {
		t.Fatalf("doubling pool slots did not shorten the wait: %.0f then %.0f", one, two)
	}
}
