package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/AshrithaG/serving-control-plane/internal/admission"
	"github.com/AshrithaG/serving-control-plane/internal/backend"
	"github.com/AshrithaG/serving-control-plane/internal/placement"
	"github.com/AshrithaG/serving-control-plane/internal/record"
)

// A backend failure must reach the client as a failure. The first version of
// the unqueued path answered 200 whatever happened, which hid every refused
// mTLS handshake from the load generator.
func TestUnqueuedPathReportsBackendFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "refused", http.StatusForbidden)
	}))
	defer dead.Close()

	rec, err := record.New(filepath.Join(t.TempDir(), "r.jsonl"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rt := New(Config{
		Mode: RoundRobin, Pool: &backend.Pool{Replicas: []*backend.Replica{backend.NewReplica("r0", dead.URL)}},
		Placement: placement.ByName("round-robin", 0), Admission: &admission.Controller{Mode: admission.Off},
		Records: rec,
	})
	body, _ := json.Marshal(map[string]any{"id": "x", "tenant": "t", "prompt": "p", "max_tokens": 4, "deadline_ms": 1000})
	w := httptest.NewRecorder()
	rt.Handle(w, httptest.NewRequest(http.MethodPost, "/generate", bytes.NewReader(body)))
	if w.Code == http.StatusOK {
		t.Fatal("router answered 200 for a request its backend refused")
	}
}
