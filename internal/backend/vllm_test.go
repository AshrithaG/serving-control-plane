package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A server that merges tokens into fewer chunks than it generated, the way vLLM
// does under load, then reports the true count in a final usage chunk.
func mergingServer(chunks, tokens int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < chunks; i++ {
			fmt.Fprint(w, `data: {"choices":[{"text":"x"}]}`+"\n\n")
		}
		fmt.Fprintf(w, `data: {"choices":[],"usage":{"completion_tokens":%d}}`+"\n\n", tokens)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func TestMergedChunksCountTheRealTokens(t *testing.T) {
	srv := mergingServer(60, 64)
	defer srv.Close()
	r := NewReplica("r0", srv.URL)
	r.Protocol = "vllm"
	res, err := r.Generate(context.Background(), "id", "prompt", 64)
	if err != nil {
		t.Fatal(err)
	}
	if res.OutTokens != 64 {
		t.Fatalf("counted %d tokens, the server generated 64 in 60 chunks", res.OutTokens)
	}
	if got := r.Outstanding(); got != 0 {
		t.Fatalf("replica still owes %d tokens after the request finished", got)
	}
}
