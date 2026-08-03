package broker

import (
	"net/http"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// requireLiveAgent is the shared authn+revocation preamble of the jail
// handlers: it authenticates the caller via mTLS (ca.RequireAgent) and denies
// a revoked identity, writing the audited 403 itself on failure. op is the
// audit-ledger operation name; detailPrefix (e.g. "consume: ") is prepended
// verbatim to the deny detail — most handlers pass "".
//
// Deliberately NOT used by gateway.go or llmproxy.go: both run isRevoked
// AFTER parsing the token id so post-revocation replays correlate with their
// mint line in the audit ledger.
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

// requireManager is the manager-only variant: authenticate the caller, require
// the manager CN, then deny a revoked manager. Order matters — the CN check
// precedes the revocation check so a non-manager caller is audited as
// "not the manager identity" regardless of revocation state.
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
