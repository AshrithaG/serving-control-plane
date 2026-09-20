// Package record writes one JSON line per request. Every number the report
// makes comes from these lines, not from anything the router prints while it
// runs.
package record

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Record is the per-request trace. Times are milliseconds since the run start
// so that a reader can line runs up without worrying about wall clocks.
type Record struct {
	ID           string  `json:"id"`
	Tenant       string  `json:"tenant"`
	Class        string  `json:"class"`
	Policy       string  `json:"policy"`
	Replica      string  `json:"replica,omitempty"`
	ArrivalMS    float64 `json:"arrival_ms"`
	AdmitMS      float64 `json:"admit_ms,omitempty"`
	DispatchMS   float64 `json:"dispatch_ms,omitempty"`
	FirstTokenMS float64 `json:"first_token_ms,omitempty"`
	LastTokenMS  float64 `json:"last_token_ms,omitempty"`
	PromptTokens int     `json:"prompt_tokens"`
	OutTokens    int     `json:"out_tokens"`
	DeadlineMS   int     `json:"deadline_ms"`
	Shed         bool    `json:"shed"`
	ShedReason   string  `json:"shed_reason,omitempty"`
	Error        string  `json:"error,omitempty"`
	PrefixHit    bool    `json:"prefix_hit"`
}

// Writer serialises records to a file. Safe for concurrent use.
type Writer struct {
	mu    sync.Mutex
	f     *os.File
	enc   *json.Encoder
	start time.Time
}

func New(path string, start time.Time) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, enc: json.NewEncoder(f), start: start}, nil
}

func (w *Writer) Since(t time.Time) float64 {
	return float64(t.Sub(w.start).Microseconds()) / 1000.0
}

func (w *Writer) Write(r Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(r)
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
