package broker

import (
	"crypto"
	"net/http"

	"github.com/stevegeek/lever/internal/wire"
)

// csrPublicKey goes through parseCSR, the only proof-of-possession check on
// /renew.
func csrPublicKey(csrPEM []byte) (crypto.PublicKey, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	return csr.PublicKey, nil
}

// handleRenew signs the CSR's public key under the AUTHENTICATED CN; the
// CSR's own CN is never used.
func (b *Broker) handleRenew(w http.ResponseWriter, r *http.Request) {
	// Denying renew to a revoked caller makes revocation terminal: its cert
	// simply expires.
	caller, ok := b.requireLiveAgent(w, r, "renew", "")
	if !ok {
		return
	}
	var req wire.RenewRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
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
	// See DirectiveStore.EnsureGeneration.
	b.directives.EnsureGeneration(caller)
	writeJSON(w, wire.RenewResponse{Cert: string(certPEM)})
	b.audit("renew", caller, "allow", "")
}
