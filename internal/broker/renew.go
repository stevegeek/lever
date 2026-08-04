package broker

import (
	"crypto"
	"encoding/json"
	"net/http"
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

// csrPublicKey parses a PEM CSR (self-signature verified by parseCSR — the
// only proof-of-possession check on /renew) and returns its public key.
func csrPublicKey(csrPEM []byte) (crypto.PublicKey, error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	return csr.PublicKey, nil
}

// handleRenew re-issues a client cert for the AUTHENTICATED caller, signing the
// CSR's public key under the authenticated CN (no CN-laundering: the CSR's own
// CN is never used).
func (b *Broker) handleRenew(w http.ResponseWriter, r *http.Request) {
	// Deny a revoked caller a fresh cert: with renew closed its existing cert
	// simply expires, fully cutting the identity off rather than letting it
	// refresh indefinitely (every use-time gate is already CN-keyed, so the
	// live cert authorizes nothing — but denying renew makes revocation terminal).
	caller, ok := b.requireLiveAgent(w, r, "renew", "")
	if !ok {
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
	// Establish this agent's directive generation if it has none yet. An agent
	// that restarts with a persisted cert (or whose cert predates the operator-
	// directive feature) refreshes via /renew and never re-hits /enrol, so
	// without this its generation stays 0 and no operator directive can target
	// it. Never bumps an existing generation — that is reserved for re-enrolment.
	b.directives.EnsureGeneration(caller)
	writeJSON(w, RenewResponse{Cert: string(certPEM)})
	b.audit("renew", caller, "allow", "")
}
