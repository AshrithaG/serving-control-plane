// Package types holds the request and record shapes shared by the router, the
// simulated backend and the load generator.
package types

import "time"

// Class is an SLO class. A request carries a deadline; the class only decides
// how the scheduler treats it when deadlines collide.
type Class string

const (
	Interactive Class = "interactive" // tight TTFT, short outputs
	Batch       Class = "batch"       // loose deadline, long outputs
)

// Request is what a client sends the router.
type Request struct {
	ID        string `json:"id"`
	Tenant    string `json:"tenant"`
	Class     Class  `json:"class"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	// DeadlineMS is the SLO: the request is useful only if the last token
	// arrives within this many milliseconds of arrival. Goodput counts the
	// requests that make it.
	DeadlineMS int `json:"deadline_ms"`
}

// PromptTokens is a deliberately crude estimate. The router never sees the
// backend's tokenizer, so every admission decision is made on an estimate, and
// the error in that estimate is one of the failure modes worth measuring.
func (r Request) PromptTokens() int {
	return len(r.Prompt)/4 + 1
}

// Deadline returns the absolute deadline given an arrival time.
func (r Request) Deadline(arrival time.Time) time.Time {
	return arrival.Add(time.Duration(r.DeadlineMS) * time.Millisecond)
}
