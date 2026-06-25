package broker

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"time"
)

// EnrolRequest is the body of POST /enrol (no client cert; ticket-authorised).
type EnrolRequest struct {
	Ticket string `json:"ticket"`
	CSR    string `json:"csr"` // PEM CSR; CN must equal the ticket's grove
}

// EnrolResponse carries the signed client cert PEM.
type EnrolResponse struct {
	Cert string `json:"cert"`
}

// csrCommonName parses a PEM CSR and returns its subject CommonName, verifying
// the CSR self-signature (proof of private-key possession).
func csrCommonName(csrPEM []byte) (string, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil {
		return "", fmt.Errorf("broker: invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return "", fmt.Errorf("broker: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", fmt.Errorf("broker: CSR signature: %w", err)
	}
	if csr.Subject.CommonName == "" {
		return "", fmt.Errorf("broker: CSR has empty common name")
	}
	return csr.Subject.CommonName, nil
}

// handleEnrol signs the CSR into a client cert IFF the request carries a valid,
// unexpired, single-use ticket whose grove EQUALS the CSR's CN. The CN==grove
// binding is what prevents any ticket from minting a cert for another identity.
// Redeem is called with the CSR's CN as the grove, so a mismatch fails and does
// NOT burn the ticket.
func (b *Broker) handleEnrol(w http.ResponseWriter, r *http.Request) {
	var req EnrolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("enrol", "", "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cn, err := csrCommonName([]byte(req.CSR))
	if err != nil {
		b.audit("enrol", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	// Bind: the ticket must have been minted for exactly this CN. A mismatch
	// returns an error and leaves the ticket intact (TicketStore.Redeem only
	// burns on a successful grove match).
	if err := b.tickets.Redeem(req.Ticket, cn, time.Now()); err != nil {
		b.audit("enrol", cn, "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	certPEM, err := b.ca.SignCSR([]byte(req.CSR))
	if err != nil {
		b.audit("enrol", cn, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(EnrolResponse{Cert: string(certPEM)})
	b.audit("enrol", cn, "allow", "")
}
