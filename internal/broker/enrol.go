package broker

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/wire"
)

// parseEnrolCSR is parseCSR (the proof-of-possession check) plus a
// non-empty CommonName.
func parseEnrolCSR(csrPEM []byte) (*x509.CertificateRequest, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	if csr.Subject.CommonName == "" {
		return nil, fmt.Errorf("broker: CSR has empty common name")
	}
	return csr, nil
}

// handleEnrol signs the CSR IFF the request carries a valid, unexpired,
// single-use ticket minted for exactly the CSR's CN: that binding stops any
// ticket from minting a cert for another identity. A mismatch does NOT burn
// the ticket.
func (b *Broker) handleEnrol(w http.ResponseWriter, r *http.Request) {
	var req wire.EnrolRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("enrol", "", "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	csr, err := parseEnrolCSR([]byte(req.CSR))
	if err != nil {
		b.audit("enrol", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	cn := csr.Subject.CommonName
	if err := b.tickets.Redeem(req.Ticket, cn, time.Now()); err != nil {
		b.audit("enrol", cn, "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	certPEM, err := b.ca.SignPublicKey(csr.PublicKey, cn)
	if err != nil {
		b.audit("enrol", cn, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// A new generation invalidates directives aimed at a previous holder of
	// a recycled slug.
	b.directives.BumpGeneration(cn)
	writeJSON(w, wire.EnrolResponse{Cert: string(certPEM)})
	b.audit("enrol", cn, "allow", "")
}
