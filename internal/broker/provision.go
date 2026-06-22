package broker

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
)

// ProvisionRequest is the body of POST /provision.
type ProvisionRequest struct {
	Grove string `json:"grove"`
}

// ProvisionResponse carries the agent's per-spawn identity material.
type ProvisionResponse struct {
	Cert    string `json:"cert"`    // client cert PEM (CN = grove)
	Key     string `json:"key"`     // client key PEM
	Biscuit string `json:"biscuit"` // base64url-encoded capability
}

// handleProvision mints a cert + biscuit for a grove. Only the manager identity
// may call it; the grant is taken strictly from policy (host-side authority).
func (b *Broker) handleProvision(w http.ResponseWriter, r *http.Request) {
	caller, err := b.callerID(r)
	if err != nil {
		b.audit("provision", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if caller != b.policy.ManagerIdentity {
		b.audit("provision", caller, "deny", "not the manager identity")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req ProvisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("provision", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	grants, ok := b.policy.GrantsFor(req.Grove)
	if !ok {
		b.audit("provision", caller, "deny", "grove not in policy: "+req.Grove)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	certPEM, keyPEM, err := b.ca.IssueAgentCert(req.Grove)
	if err != nil {
		b.audit("provision", caller, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tok, err := token.Mint(b.keys.Private, token.Grant{
		Agent:  req.Grove,
		Tools:  grants,
		Expiry: time.Now().Add(b.policy.GrantTTL),
		Epoch:  b.MinEpoch(),
	})
	if err != nil {
		b.audit("provision", caller, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ProvisionResponse{
		Cert:    string(certPEM),
		Key:     string(keyPEM),
		Biscuit: base64.RawURLEncoding.EncodeToString(tok),
	})
	b.audit("provision", caller, "allow", req.Grove)
}
