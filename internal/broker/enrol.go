package broker

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/wire"
)

// parseEnrolCSR parses a PEM CSR (self-signature verified by parseCSR — the
// proof-of-possession check) and requires a non-empty subject CommonName.
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

// handleEnrol signs the CSR into a client cert IFF the request carries a valid,
// unexpired, single-use ticket whose worker EQUALS the CSR's CN. The CN==worker
// binding is what prevents any ticket from minting a cert for another identity.
// Redeem is called with the CSR's CN as the worker, so a mismatch fails and does
// NOT burn the ticket.
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
	// Bind: the ticket must have been minted for exactly this CN. A mismatch
	// returns an error and leaves the ticket intact (TicketStore.Redeem only
	// burns on a successful worker match).
	if err := b.tickets.Redeem(req.Ticket, cn, time.Now()); err != nil {
		b.audit("enrol", cn, "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// The CSR was parsed and signature-checked above; sign its public key
	// under the CN the ticket was redeemed for (the same call /renew makes).
	certPEM, err := b.ca.SignPublicKey(csr.PublicKey, cn)
	if err != nil {
		b.audit("enrol", cn, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// A successful enrolment starts a new directive generation for this CN:
	// directives are bound to (CN, generation), so anything targeted at a
	// previous holder of a recycled slug is invalidated here.
	b.directives.BumpGeneration(cn)
	writeJSON(w, wire.EnrolResponse{Cert: string(certPEM)})
	b.audit("enrol", cn, "allow", "")
}
