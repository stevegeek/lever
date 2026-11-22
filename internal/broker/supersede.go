package broker

import (
	"crypto/x509"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/cap/ca"
)

// Supersede records that name's scion record is gone: from now on a client
// certificate of that name issued before this call is refused on every jail
// route (supersededGate), so a copy of the deleted agent's key and leaf
// carries no authority, while the name enrols afresh. Revoke, by contrast,
// ends the name for good. Call it only once the record is confirmed gone: a
// record resumed later would boot with a leaf the broker now refuses.
//
// The cutoff is the next whole second, since a leaf's issue time is known
// only to the second: older leaves of the name fall before it, and leafFrom
// stamps any leaf issued until then at the cutoff. The error is the save's:
// the cutoff holds in this process either way, and is lost at a restart if
// unsaved.
func (b *Broker) Supersede(name string) error {
	cutoff := b.now().Truncate(time.Second).Add(time.Second)
	b.mu.Lock()
	if b.superseded == nil {
		b.superseded = map[string]time.Time{}
	}
	b.superseded[name] = cutoff
	err := b.saveRevocationLocked()
	b.mu.Unlock()
	return err
}

// leafFrom is the earliest issue time a new leaf of name may carry: its
// cutoff, or zero.
func (b *Broker) leafFrom(name string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.superseded[name]
}

// leafSuperseded reports whether leaf was issued before its name's cutoff.
func (b *Broker) leafSuperseded(leaf *x509.Certificate) bool {
	b.mu.Lock()
	cutoff, ok := b.superseded[leaf.Subject.CommonName]
	b.mu.Unlock()
	return ok && ca.LeafIssuedAt(leaf).Before(cutoff)
}

// supersededGate refuses every request whose client leaf a purge superseded,
// before any route: the gateway and the llm proxy read only the CN, as do
// renew and minting.
func (b *Broker) supersededGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 && b.leafSuperseded(r.TLS.PeerCertificates[0]) {
			b.audit("auth", r.TLS.PeerCertificates[0].Subject.CommonName, "deny", "client certificate issued before its agent's record was purged")
			w.Header().Set("Connection", "close")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
