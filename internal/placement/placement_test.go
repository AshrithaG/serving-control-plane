package placement

import (
	"context"
	"testing"

	"github.com/AshrithaG/serving-control-plane/internal/backend"
	"github.com/AshrithaG/serving-control-plane/internal/types"
)

func pool(n int) *backend.Pool {
	p := &backend.Pool{}
	for i := 0; i < n; i++ {
		p.Replicas = append(p.Replicas, backend.NewReplica(string(rune('a'+i)), "http://x"))
	}
	return p
}

func TestPrefixAffinityIsStable(t *testing.T) {
	p := &PrefixAffinity{Imbalance: 4}
	pl := pool(3)
	req := types.Request{Prompt: "a long shared system prompt that several requests share verbatim"}
	first := p.Pick(context.Background(), pl, req)
	for i := 0; i < 50; i++ {
		if got := p.Pick(context.Background(), pl, req); got != first {
			t.Fatalf("same prefix landed on %s then %s", first.Name, got.Name)
		}
	}
}

func TestRoundRobinCoversEveryReplica(t *testing.T) {
	p := &RoundRobin{}
	pl := pool(3)
	seen := map[string]int{}
	for i := 0; i < 30; i++ {
		seen[p.Pick(context.Background(), pl, types.Request{}).Name]++
	}
	if len(seen) != 3 {
		t.Fatalf("round robin used %d of 3 replicas: %v", len(seen), seen)
	}
	for name, n := range seen {
		if n != 10 {
			t.Fatalf("replica %s got %d of 30, want 10", name, n)
		}
	}
}
