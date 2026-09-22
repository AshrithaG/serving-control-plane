// Package backend holds the router's view of one inference engine: how to send
// it work, what it reports about itself, and what it has actually cost so far.
package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Stats mirrors what vLLM exposes about queue depth and KV usage. The stand-in
// backend serves the same shape so the router code does not change when it is
// pointed at a real engine.
type Stats struct {
	Running     int     `json:"running"`
	Waiting     int     `json:"waiting"`
	MaxRunning  int     `json:"max_running"`
	CacheUsage  float64 `json:"cache_usage"`
	CachedItems int     `json:"cached_items"`
}

// Replica is one engine. The EWMA fields are the router's learned model of it:
// nothing is configured from the engine's own constants, because in production
// the router does not know them either.
type Replica struct {
	Name string
	URL  string
	// Protocol is "sim" for the stand-in engine or "vllm" for a real one.
	Protocol string

	client *http.Client

	mu           sync.Mutex
	inflight     int
	// outstanding is token debt: what has been promised to clients on this
	// replica and not yet generated. Counting requests instead, and charging
	// each one the new request's full length, badly overestimates when the
	// running requests are nearly finished.
	outstanding  int64
	decodeMS     float64 // EWMA milliseconds per output token, as observed
	prefillUS    float64 // EWMA microseconds per prompt token, as observed
	observations int
	lastStats    Stats
	lastStatsAt  time.Time
}

func NewReplica(name, url string) *Replica {
	return &Replica{
		Name: name, URL: url, Protocol: "sim",
		client: &http.Client{Timeout: 10 * time.Minute},
		// Seeds only. They are replaced by measurement after a few requests;
		// the seed exists so the first admission decision is not a divide by
		// zero, and the error it introduces is visible in the records.
		decodeMS: 10, prefillUS: 150,
	}
}

// SetTransport swaps the HTTP transport, used to put mTLS under every call.
func (r *Replica) SetTransport(rt http.RoundTripper) { r.client.Transport = rt }

func (r *Replica) Inflight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight
}

// Outstanding is the token debt still owed by this replica.
func (r *Replica) Outstanding() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outstanding
}

func (r *Replica) addOutstanding(n int64) {
	r.mu.Lock()
	r.outstanding += n
	if r.outstanding < 0 {
		r.outstanding = 0
	}
	r.mu.Unlock()
}

// Estimate returns the router's current guess at prefill and decode cost.
func (r *Replica) Estimate() (prefillUSPerTok, decodeMSPerTok float64, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prefillUS, r.decodeMS, r.observations
}

func (r *Replica) observe(promptTokens, outTokens int, prefill, decode time.Duration) {
	if promptTokens <= 0 || outTokens <= 0 {
		return
	}
	const w = 0.2 // EWMA weight: fast enough to track a load change, slow
	// enough that one slow request does not swing admission.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefillUS = (1-w)*r.prefillUS + w*(float64(prefill.Microseconds())/float64(promptTokens))
	r.decodeMS = (1-w)*r.decodeMS + w*(float64(decode.Milliseconds())/float64(outTokens))
	r.observations++
}

// Stats fetches engine-reported state, cached for 50ms so that a burst of
// arrivals does not turn into a burst of stats calls.
func (r *Replica) Stats(ctx context.Context) Stats {
	if r.Protocol == "vllm" {
		r.mu.Lock()
		if time.Since(r.lastStatsAt) < 50*time.Millisecond {
			s := r.lastStats
			r.mu.Unlock()
			return s
		}
		r.mu.Unlock()
		s := r.statsVLLM(ctx)
		r.mu.Lock()
		r.lastStats, r.lastStatsAt = s, time.Now()
		r.mu.Unlock()
		return s
	}
	r.mu.Lock()
	if time.Since(r.lastStatsAt) < 50*time.Millisecond {
		s := r.lastStats
		r.mu.Unlock()
		return s
	}
	r.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL+"/stats", nil)
	if err != nil {
		return Stats{}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Stats{}
	}
	defer resp.Body.Close()
	var s Stats
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return Stats{}
	}
	r.mu.Lock()
	r.lastStats, r.lastStatsAt = s, time.Now()
	r.mu.Unlock()
	return s
}

// Result is what one generation cost, as observed by the router.
type Result struct {
	FirstToken time.Time
	LastToken  time.Time
	OutTokens  int
	PrefixHit  bool
}

// Generate streams one request and reports when the first and last tokens
// arrived. Time to first token is measured at the router, not at the engine,
// so queueing inside the engine is included where it belongs.
func (r *Replica) Generate(ctx context.Context, id, prompt string, maxTokens int) (Result, error) {
	return r.GenerateWithPriority(ctx, id, prompt, maxTokens, nil)
}

// GenerateWithPriority is Generate with an engine-side priority, used only on
// the vLLM protocol; the simulated engine has no priority scheduler.
func (r *Replica) GenerateWithPriority(ctx context.Context, id, prompt string, maxTokens int, priority *int64) (Result, error) {
	if r.Protocol == "vllm" {
		r.mu.Lock()
		r.inflight++
		r.outstanding += int64(maxTokens)
		r.mu.Unlock()
		res, err := r.generateVLLM(ctx, id, prompt, maxTokens, priority)
		r.mu.Lock()
		r.inflight--
		r.outstanding -= int64(maxTokens - res.OutTokens)
		if r.outstanding < 0 {
			r.outstanding = 0
		}
		r.mu.Unlock()
		return res, err
	}
	body, _ := json.Marshal(map[string]any{"id": id, "prompt": prompt, "max_tokens": maxTokens})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/generate", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	r.mu.Lock()
	r.inflight++
	r.outstanding += int64(maxTokens)
	r.mu.Unlock()
	remaining := int64(maxTokens)
	defer func() {
		r.mu.Lock()
		r.inflight--
		r.outstanding -= remaining
		if r.outstanding < 0 {
			r.outstanding = 0
		}
		r.mu.Unlock()
	}()

	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("backend %s: status %d", r.Name, resp.StatusCode)
	}

	var res Result
	res.PrefixHit = resp.Header.Get("X-Prefix-Hit") == "true"
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		now := time.Now()
		if res.OutTokens == 0 {
			res.FirstToken = now
		}
		res.OutTokens++
		res.LastToken = now
		if remaining > 0 {
			remaining--
			r.addOutstanding(-1)
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if res.OutTokens == 0 {
		return res, fmt.Errorf("backend %s: no tokens", r.Name)
	}
	r.observe(len(prompt)/4+1, res.OutTokens, res.FirstToken.Sub(start), res.LastToken.Sub(res.FirstToken))
	return res, nil
}

// Pool is the set of replicas behind the router.
type Pool struct{ Replicas []*Replica }

func (p *Pool) ByName(name string) *Replica {
	for _, r := range p.Replicas {
		if r.Name == name {
			return r
		}
	}
	return nil
}
