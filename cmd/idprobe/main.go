// Command idprobe tries to call a backend the ways an attacker inside the
// cluster could, and reports whether each attempt got through. It is the
// negative half of the identity test: proving the router can connect is only
// interesting next to proof that nothing else can.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"

	"github.com/AshrithaG/serving-control-plane/internal/identity"
)

func main() {
	target := flag.String("target", "https://backend-0:8100/stats", "URL to call")
	socket := flag.String("spiffe-socket", "", "Workload API socket, for the svid attempt")
	flag.Parse()

	type result struct {
		Attempt string `json:"attempt"`
		GotIn   bool   `json:"got_in"`
		Detail  string `json:"detail"`
	}
	enc := json.NewEncoder(os.Stdout)
	try := func(name string, c *http.Client, url string) {
		resp, err := c.Get(url)
		if err != nil {
			enc.Encode(result{name, false, err.Error()})
			return
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 120))
		resp.Body.Close()
		enc.Encode(result{name, resp.StatusCode == 200, resp.Status + " " + string(body)})
	}
	timeout := 5 * time.Second

	// 1. Plain HTTP against the TLS port.
	plain := "http" + (*target)[len("https"):]
	try("plaintext", &http.Client{Timeout: timeout}, plain)

	// 2. TLS without a client certificate.
	try("tls_no_client_cert", &http.Client{Timeout: timeout, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the probe is the attacker
	}}, *target)

	// 3. A valid SVID from the same trust domain, for the wrong workload. This
	// is the case that separates authorization from mere authentication.
	if *socket != "" {
		src, err := identity.Source(context.Background(), *socket)
		if err != nil {
			enc.Encode(result{"valid_svid_wrong_identity", false, "no SVID: " + err.Error()})
			return
		}
		defer src.Close()
		svid, _ := src.GetX509SVID()
		cfg := tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeAny())
		// Run as the intruder this must be refused; run as the router it must
		// get in. The label names the identity rather than presuming the verdict.
		name := "own_svid"
		if svid != nil {
			name += " (" + svid.ID.String() + ")"
		}
		try(name, &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: cfg}}, *target)
	}
}
