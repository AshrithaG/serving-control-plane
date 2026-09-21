// Package identity gives the router and the backends SPIFFE identities and
// uses them for mutual TLS with explicit authorization.
//
// The point is the authorization, not the encryption. mTLS with any valid
// certificate from the trust domain only proves the peer belongs to the
// cluster; it says nothing about whether it is the router. Every connection
// here checks the peer's SPIFFE ID against the one identity allowed to make
// that call, so a compromised pod elsewhere in the cluster, holding a
// perfectly valid identity of its own, is still refused.
package identity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

// Source fetches and rotates this workload's X.509 SVID from the SPIRE agent's
// Workload API. Rotation is automatic: the source swaps in the new certificate
// before the old one expires, and new handshakes use it.
func Source(ctx context.Context, socket string) (*workloadapi.X509Source, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return workloadapi.NewX509Source(ctx,
		workloadapi.WithClientOptions(workloadapi.WithAddr(socket)))
}

// ServerConfig accepts only peers presenting one of the allowed SPIFFE IDs.
func ServerConfig(src *workloadapi.X509Source, allowed ...string) (*tls.Config, error) {
	ids, err := parse(allowed)
	if err != nil {
		return nil, err
	}
	return tlsconfig.MTLSServerConfig(src, src, tlsconfig.AuthorizeOneOf(ids...)), nil
}

// ClientConfig connects only to a server presenting the expected SPIFFE ID, so
// the router cannot be pointed at an impostor backend either.
func ClientConfig(src *workloadapi.X509Source, expected ...string) (*tls.Config, error) {
	ids, err := parse(expected)
	if err != nil {
		return nil, err
	}
	return tlsconfig.MTLSClientConfig(src, src, tlsconfig.AuthorizeOneOf(ids...)), nil
}

// PeerID reads the verified SPIFFE ID of the caller from a request.
func PeerID(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	id, err := x509svid.IDFromCert(r.TLS.PeerCertificates[0])
	if err != nil {
		return ""
	}
	return id.String()
}

// WatchRotation logs every time this workload's own certificate changes, which
// is the evidence that rotation happened while traffic kept flowing.
func WatchRotation(ctx context.Context, src *workloadapi.X509Source, who string) {
	var last string
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			svid, err := src.GetX509SVID()
			if err != nil || len(svid.Certificates) == 0 {
				continue
			}
			c := svid.Certificates[0]
			serial := c.SerialNumber.String()
			if serial != last {
				log.Printf("svid %s: %s serial=%s expires=%s", who, svid.ID, short(serial), c.NotAfter.Format(time.RFC3339))
				last = serial
			}
		}
	}
}

// Serial returns a short form of a certificate serial for logs.
func Serial(c *x509.Certificate) string { return short(c.SerialNumber.String()) }

func short(s string) string {
	if len(s) > 10 {
		return s[len(s)-10:]
	}
	return s
}

func parse(raw []string) ([]spiffeid.ID, error) {
	var ids []spiffeid.ID
	for _, r := range raw {
		id, err := spiffeid.FromString(r)
		if err != nil {
			return nil, fmt.Errorf("bad SPIFFE ID %q: %w", r, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
