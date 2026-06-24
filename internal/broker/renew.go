package broker

import (
	"crypto"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"

	"github.com/lever-to/lever/internal/cap/ca"
)

// RenewRequest carries a fresh CSR (new keypair). Its CN is IGNORED; the renewed
// cert always carries the caller's authenticated CN.
type RenewRequest struct {
	CSR string `json:"csr"`
}

// RenewResponse carries the renewed client cert PEM.
type RenewResponse struct {
	Cert string `json:"cert"`
}

// csrPublicKey parses a PEM CSR, verifies its self-signature (proof of
// private-key possession), and returns its public key.
func csrPublicKey(csrPEM []byte) (crypto.PublicKey, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil {
		return nil, fmt.Errorf("broker: invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("broker: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("broker: CSR signature: %w", err)
	}
	return csr.PublicKey, nil
}

// handleRenew re-issues a client cert for the AUTHENTICATED caller, signing the
// CSR's public key under the authenticated CN (no CN-laundering: the CSR's own
// CN is never used).
func (b *Broker) handleRenew(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("renew", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req RenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("renew", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	pub, err := csrPublicKey([]byte(req.CSR))
	if err != nil {
		b.audit("renew", caller, "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	certPEM, err := b.ca.SignPublicKey(pub, caller)
	if err != nil {
		b.audit("renew", caller, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(RenewResponse{Cert: string(certPEM)})
	b.audit("renew", caller, "allow", "")
}
