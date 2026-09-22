package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The vLLM protocol: OpenAI-style streaming completions plus the engine's own
// Prometheus metrics for queue depth and KV usage. The simulator speaks a
// simpler protocol; everything above this file is the same for both, which is
// what makes the simulated and measured numbers comparable at all.

// Model is the served model name vLLM expects in each request.
var Model = "Qwen/Qwen3-1.7B"

func (r *Replica) generateVLLM(ctx context.Context, id, prompt string, maxTokens int, priority *int64) (Result, error) {
	payload := map[string]any{
		"model": Model, "prompt": prompt, "max_tokens": maxTokens,
		"stream": true, "temperature": 0,
		// Under load vLLM can merge several tokens into one streamed chunk, so
		// counting chunks undercounts tokens: the 2026-09-21 saturation run saw
		// 2.4% of unqueued requests short by 1 to 23 tokens. The final usage chunk
		// carries the exact count.
		"stream_options": map[string]any{"include_usage": true},
		// Without this, a short answer ends early and the request costs less
		// than the router budgeted, which quietly flatters every policy.
		"ignore_eos": true,
	}
	if priority != nil {
		// Lower is served first, and the engine must run with
		// --scheduling-policy priority or it rejects a non-zero value.
		payload["priority"] = *priority
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/v1/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("vllm %s: status %d", r.Name, resp.StatusCode)
	}

	var res Result
	exact := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Text string `json:"text"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil && chunk.Usage.CompletionTokens > 0 {
			exact = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		// One streamed chunk is one decode step for a single sequence, which
		// is what TBT is meant to measure.
		now := time.Now()
		if res.OutTokens == 0 {
			res.FirstToken = now
		}
		res.OutTokens++
		res.LastToken = now
		r.addOutstanding(-1)
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if res.OutTokens == 0 {
		return res, fmt.Errorf("vllm %s: no tokens", r.Name)
	}
	if exact > res.OutTokens {
		// Token debt was paid down once per chunk; pay the merged tokens too, or
		// the router believes the replica still owes work it has finished.
		r.addOutstanding(-int64(exact - res.OutTokens))
		res.OutTokens = exact
	}
	r.observe(len(prompt)/4+1, res.OutTokens, res.FirstToken.Sub(start), res.LastToken.Sub(res.FirstToken))
	return res, nil
}

// statsVLLM reads the engine's Prometheus metrics. Names follow vLLM's V1
// engine; a missing metric reads as zero rather than failing the request.
func (r *Replica) statsVLLM(ctx context.Context) Stats {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL+"/metrics", nil)
	if err != nil {
		return Stats{}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Stats{}
	}
	defer resp.Body.Close()
	var s Stats
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, val, ok := metricLine(line)
		if !ok {
			continue
		}
		switch name {
		case "vllm:num_requests_running":
			s.Running = int(val)
		case "vllm:num_requests_waiting":
			s.Waiting = int(val)
		case "vllm:gpu_cache_usage_perc", "vllm:kv_cache_usage_perc":
			s.CacheUsage = val
		}
	}
	s.MaxRunning = MaxRunningVLLM
	return s
}

// MaxRunningVLLM mirrors the engine's --max-num-seqs, which vLLM does not
// export as a metric. The run script sets both from one variable.
var MaxRunningVLLM = 32

func metricLine(line string) (string, float64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	name := fields[0]
	if i := strings.IndexByte(name, '{'); i >= 0 {
		name = name[:i]
	}
	v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		return "", 0, false
	}
	return name, v, true
}
