// Command router runs the control plane in one of four configurations so that
// they can be compared on the same workload and the same backends.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AshrithaG/serving-control-plane/internal/admission"
	"github.com/AshrithaG/serving-control-plane/internal/backend"
	"github.com/AshrithaG/serving-control-plane/internal/identity"
	"github.com/AshrithaG/serving-control-plane/internal/placement"
	"github.com/AshrithaG/serving-control-plane/internal/record"
	"github.com/AshrithaG/serving-control-plane/internal/router"
)

// parseBackends reads "r0=http://127.0.0.1:8100,r1=http://127.0.0.1:8101".
func parseBackends(s string) *backend.Pool {
	p := &backend.Pool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		if !ok {
			name, url = part, part
		}
		p.Replicas = append(p.Replicas, backend.NewReplica(name, url))
	}
	return p
}

// parseWeights reads "acme=2,globex=1".
func parseWeights(s string) map[string]float64 {
	m := map[string]float64{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		name, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		w, err := strconv.ParseFloat(val, 64)
		if err != nil {
			log.Fatalf("bad weight %q: %v", part, err)
		}
		m[name] = w
	}
	return m
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	mode := flag.String("mode", "full", "direct | rr | fifo | full | edf")
	backends := flag.String("backends", "r0=http://127.0.0.1:8100", "name=url pairs")
	place := flag.String("placement", "prefix-affinity", "round-robin | least-loaded | prefix-affinity")
	imbalance := flag.Int("imbalance", 2, "in-flight gap at which prefix affinity gives way to load")
	admit := flag.String("admission", "deadline", "off | deadline")
	safety := flag.Float64("safety", 1.0, "scales the completion estimate before comparing to the deadline")
	quantum := flag.Float64("quantum", 256, "deficit round robin quantum, in tokens")
	weights := flag.String("weights", "", "tenant=weight pairs, default weight 1")
	maxInflight := flag.Int("max-inflight", 8, "requests the router will keep dispatched at once")
	records := flag.String("records", "run.jsonl", "per-request records")
	protocol := flag.String("protocol", "sim", "sim | vllm")
	model := flag.String("model", backend.Model, "served model name, vllm protocol only")
	maxSeqs := flag.Int("max-num-seqs", backend.MaxRunningVLLM, "the engines' --max-num-seqs, vllm protocol only")
	socket := flag.String("spiffe-socket", "", "SPIRE agent Workload API socket; enables mTLS to backends")
	backendID := flag.String("backend-id", "", "SPIFFE ID every backend must present")
	flag.Parse()
	backend.Model = *model
	backend.MaxRunningVLLM = *maxSeqs

	m := router.Mode(*mode)
	pl := placement.ByName(*place, *imbalance)
	if m == router.Direct || m == router.RoundRobin {
		// Neither baseline is allowed a smart placement policy: that is what
		// makes them baselines.
		pl = placement.ByName("round-robin", 0)
	}
	adm := &admission.Controller{Mode: admission.Mode(*admit), Safety: *safety}
	if m != router.Full && m != router.EDF {
		adm.Mode = admission.Off
	}

	rec, err := record.New(*records, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	defer rec.Close()

	pool := parseBackends(*backends)
	for _, r := range pool.Replicas {
		r.Protocol = *protocol
	}
	if *socket != "" {
		src, err := identity.Source(context.Background(), *socket)
		if err != nil {
			log.Fatalf("workload API: %v", err)
		}
		defer src.Close()
		go identity.WatchRotation(context.Background(), src, "router")
		cfg, err := identity.ClientConfig(src, *backendID)
		if err != nil {
			log.Fatal(err)
		}
		for _, r := range pool.Replicas {
			r.SetTransport(&http.Transport{
				TLSClientConfig: cfg.Clone(), MaxIdleConnsPerHost: 64,
				// Idle connections are closed after 10s so new handshakes, and
				// with them rotated certificates, happen during a long run.
				IdleConnTimeout: 10 * time.Second, ForceAttemptHTTP2: false,
				TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
			})
		}
		log.Printf("mTLS to backends, requiring %s", *backendID)
	}
	rt := router.New(router.Config{
		Mode: m, Pool: pool, Placement: pl, Admission: adm,
		Quantum: *quantum, Weights: parseWeights(*weights),
		MaxInflight: *maxInflight, Records: rec,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rt.Run(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/generate", rt.Handle)
	mux.HandleFunc("/stats", rt.Stats)
	log.Printf("router mode=%s placement=%s admission=%s max-inflight=%d -> %s",
		m, pl.Name(), adm.Mode, *maxInflight, *records)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
