// Command fakebackend stands in for a vLLM engine so the router's policies can
// be developed and tested without a GPU.
//
// It is a model of an engine, not an engine. It reproduces the four behaviours
// the router actually reasons about: prefill cost proportional to prompt
// length, decode slowing down as concurrency rises, a bounded number of
// concurrent sequences (the KV budget), and a prefix cache that makes a repeat
// prefix cheap. Every number the router's report claims must come from a real
// engine; this exists so that the scheduling logic is already correct when it
// gets there.
package main

import (
	"container/list"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"sync"
	"time"
)

type genRequest struct {
	ID        string `json:"id"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
}

type stats struct {
	Running     int     `json:"running"`
	Waiting     int     `json:"waiting"`
	MaxRunning  int     `json:"max_running"`
	CacheUsage  float64 `json:"cache_usage"`
	CachedItems int     `json:"cached_items"`
}

type engine struct {
	mu          sync.Mutex
	running     int
	waiting     int
	maxRunning  int
	prefillUS   float64 // microseconds per prompt token
	decodeMS    float64 // milliseconds per output token at concurrency 1
	alpha       float64 // decode slowdown per extra concurrent sequence
	cacheHitMul float64 // prefill multiplier on a prefix-cache hit
	cacheCap    int
	cache       map[uint64]*list.Element
	lru         *list.List
	admit       chan struct{}
}

func newEngine(maxRunning int, prefillUS, decodeMS, alpha, hitMul float64, cacheCap int) *engine {
	return &engine{
		maxRunning: maxRunning, prefillUS: prefillUS, decodeMS: decodeMS,
		alpha: alpha, cacheHitMul: hitMul, cacheCap: cacheCap,
		cache: map[uint64]*list.Element{}, lru: list.New(),
		admit: make(chan struct{}, maxRunning),
	}
}

// prefixKey hashes the first 64 bytes of the prompt. A real engine keys its
// cache on token blocks; the router only needs the property that identical
// leading context is cheap, so the stand-in keeps that and nothing else.
func prefixKey(prompt string) uint64 {
	n := 64
	if len(prompt) < n {
		n = len(prompt)
	}
	h := fnv.New64a()
	h.Write([]byte(prompt[:n]))
	return h.Sum64()
}

func (e *engine) touchPrefix(k uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if el, ok := e.cache[k]; ok {
		e.lru.MoveToFront(el)
		return true
	}
	el := e.lru.PushFront(k)
	e.cache[k] = el
	for e.lru.Len() > e.cacheCap {
		last := e.lru.Back()
		e.lru.Remove(last)
		delete(e.cache, last.Value.(uint64))
	}
	return false
}

func (e *engine) stats() stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return stats{
		Running: e.running, Waiting: e.waiting, MaxRunning: e.maxRunning,
		CacheUsage:  float64(e.lru.Len()) / float64(e.cacheCap),
		CachedItems: e.lru.Len(),
	}
}

func (e *engine) handleGenerate(w http.ResponseWriter, r *http.Request) {
	var req genRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hit := e.touchPrefix(prefixKey(req.Prompt))

	e.mu.Lock()
	e.waiting++
	e.mu.Unlock()
	select {
	case e.admit <- struct{}{}: // a KV slot
	case <-r.Context().Done():
		e.mu.Lock()
		e.waiting--
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	e.waiting--
	e.running++
	conc := e.running
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.running--
		e.mu.Unlock()
		<-e.admit
	}()

	promptTokens := len(req.Prompt)/4 + 1
	prefill := time.Duration(e.prefillUS*float64(promptTokens)) * time.Microsecond
	if hit {
		prefill = time.Duration(float64(prefill) * e.cacheHitMul)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Prefix-Hit", fmt.Sprintf("%t", hit))
	w.WriteHeader(http.StatusOK)

	select {
	case <-time.After(prefill):
	case <-r.Context().Done():
		return
	}

	enc := json.NewEncoder(w)
	for i := 0; i < req.MaxTokens; i++ {
		// Re-read concurrency each token: a request that arrives mid-decode
		// slows everyone already running, which is the effect the router's
		// admission control exists to bound.
		e.mu.Lock()
		conc = e.running
		e.mu.Unlock()
		step := time.Duration(e.decodeMS*(1+e.alpha*float64(conc-1))) * time.Millisecond
		select {
		case <-time.After(step):
		case <-r.Context().Done():
			return
		}
		if err := enc.Encode(map[string]any{"i": i, "done": i == req.MaxTokens-1}); err != nil {
			return
		}
		flusher.Flush()
	}
}

func main() {
	addr := flag.String("addr", ":8100", "listen address")
	maxRunning := flag.Int("max-running", 8, "concurrent sequences the KV budget allows")
	prefillUS := flag.Float64("prefill-us", 120, "microseconds of prefill per prompt token")
	decodeMS := flag.Float64("decode-ms", 8, "milliseconds per output token at concurrency 1")
	alpha := flag.Float64("alpha", 0.06, "decode slowdown per extra concurrent sequence")
	hitMul := flag.Float64("cache-hit-mul", 0.15, "prefill multiplier on a prefix-cache hit")
	cacheCap := flag.Int("cache-cap", 64, "prefixes the cache holds")
	flag.Parse()

	e := newEngine(*maxRunning, *prefillUS, *decodeMS, *alpha, *hitMul, *cacheCap)
	mux := http.NewServeMux()
	mux.HandleFunc("/generate", e.handleGenerate)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(e.stats())
	})
	log.Printf("fakebackend on %s (max-running=%d decode-ms=%.1f alpha=%.2f)", *addr, *maxRunning, *decodeMS, *alpha)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
