// Command loadgen offers work to the router open loop: arrivals follow a
// Poisson process and do not wait for earlier requests to finish. A closed-loop
// generator cannot overload the system it is measuring, which is exactly the
// regime admission control exists for, so the arrival process is the part that
// has to be right.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type tenantSpec struct {
	name  string
	share float64
}

func parseTenants(s string) []tenantSpec {
	var out []tenantSpec
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			out = append(out, tenantSpec{name: part, share: 1})
			continue
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			log.Fatalf("bad tenant share %q: %v", part, err)
		}
		out = append(out, tenantSpec{name: name, share: f})
	}
	return out
}

func main() {
	target := flag.String("target", "http://127.0.0.1:8080/generate", "router endpoint")
	rate := flag.Float64("rate", 12, "arrivals per second")
	dur := flag.Duration("duration", 30*time.Second, "how long to offer load")
	tenants := flag.String("tenants", "acme=1,globex=1", "tenant=share pairs")
	sharedPrompts := flag.Int("shared-prompts", 8, "distinct system prompts in the pool")
	prefixShare := flag.Float64("prefix-share", 0.7, "fraction of requests that reuse a pooled prompt")
	interactiveShare := flag.Float64("interactive-share", 0.7, "fraction of interactive requests")
	interactiveTokens := flag.Int("interactive-tokens", 64, "output tokens for interactive requests")
	batchTokens := flag.Int("batch-tokens", 384, "output tokens for batch requests")
	interactiveSLO := flag.Int("interactive-slo-ms", 3000, "deadline for interactive requests")
	batchSLO := flag.Int("batch-slo-ms", 20000, "deadline for batch requests")
	seed := flag.Int64("seed", 1, "arrival and content seed")
	flag.Parse()

	rng := rand.New(rand.NewSource(*seed))
	ts := parseTenants(*tenants)
	total := 0.0
	for _, t := range ts {
		total += t.share
	}

	pool := make([]string, *sharedPrompts)
	for i := range pool {
		pool[i] = fmt.Sprintf("system prompt %02d: %s", i, strings.Repeat("context ", 40))
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	var sent, ok200, shed429, errs atomic.Int64
	var wg sync.WaitGroup
	deadline := time.Now().Add(*dur)
	id := 0

	for time.Now().Before(deadline) {
		// Exponential gaps give a Poisson process; a fixed sleep would
		// understate queueing badly.
		gap := time.Duration(rng.ExpFloat64() / *rate * float64(time.Second))
		time.Sleep(gap)

		pick, acc := rng.Float64()*total, 0.0
		tenant := ts[len(ts)-1].name
		for _, t := range ts {
			if acc += t.share; pick <= acc {
				tenant = t.name
				break
			}
		}
		prompt := fmt.Sprintf("unique %d %s", id, strings.Repeat("filler ", 10))
		if rng.Float64() < *prefixShare {
			prompt = pool[rng.Intn(len(pool))] + fmt.Sprintf(" question %d", id)
		}
		class, maxTok, slo := "interactive", *interactiveTokens, *interactiveSLO
		if rng.Float64() >= *interactiveShare {
			class, maxTok, slo = "batch", *batchTokens, *batchSLO
		}
		body, _ := json.Marshal(map[string]any{
			"id": fmt.Sprintf("req-%05d", id), "tenant": tenant, "class": class,
			"prompt": prompt, "max_tokens": maxTok, "deadline_ms": slo,
		})
		id++
		sent.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Post(*target, "application/json", bytes.NewReader(body))
			if err != nil {
				errs.Add(1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK:
				ok200.Add(1)
			case http.StatusTooManyRequests:
				shed429.Add(1)
			default:
				errs.Add(1)
			}
		}()
	}
	wg.Wait()
	log.Printf("sent=%d ok=%d shed=%d errors=%d", sent.Load(), ok200.Load(), shed429.Load(), errs.Load())
}
