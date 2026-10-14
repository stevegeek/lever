package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// requireLiveAgent authenticates the caller via mTLS and denies a revoked
// identity, writing the audited 403 itself.
//
// Deliberately NOT used by the gateway or the llm proxy: they check
// revocation AFTER parsing the token id, so a replay after revocation ties to
// its mint line.
func (b *Broker) requireLiveAgent(w http.ResponseWriter, r *http.Request, op, detailPrefix string) (string, bool) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit(op, "", "deny", detailPrefix+err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	if b.isRevoked(caller) {
		b.audit(op, caller, "deny", detailPrefix+"revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	return caller, true
}

// requireManager checks the manager CN before revocation, so a non-manager
// is audited as such whatever its revocation state.
func (b *Broker) requireManager(w http.ResponseWriter, r *http.Request, op, detailPrefix string) (string, bool) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit(op, "", "deny", detailPrefix+err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	if caller != b.manager {
		b.audit(op, caller, "deny", detailPrefix+"not the manager identity")
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	if b.isRevoked(caller) {
		b.audit(op, caller, "deny", detailPrefix+"revoked")
		http.Error(w, "forbidden", http.StatusForbidden)
		return "", false
	}
	return caller, true
}
