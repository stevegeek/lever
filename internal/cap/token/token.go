// Mint/Verify for the capability token: a compact Ed25519-signed struct that
// binds one (tool, operation) capability to a single agent identity, with
// optional parameter constraints, a hard expiry, and a mint epoch (revocation
// floor). It replaced a biscuit/Datalog token: the only biscuit-specific
// feature — offline holder-side attenuation — was unused, because every
// capability is minted online by the broker, which is always on the
// verification path. A typed signed struct carries everything actually relied
// upon (bound agent, capability match, equality constraints, expiry, epoch)
// with one fewer dependency and no policy interpreter.

package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Capability is the single (tool, operation) a token authorizes.
type Capability struct {
	Tool      string
	Operation string
}

// Constraint binds a request parameter to a fixed value. Verification requires
// the request to carry that exact (key, value) parameter, so a constraint can
// only narrow what a request may do, never widen it.
type Constraint struct {
	Key   string
	Value string
}

// Grant is the capability the broker mints for one agent.
type Grant struct {
	Agent       string       // bound_agent: the only identity that may exercise it
	Capability  Capability   // the single (tool, operation) granted
	Constraints []Constraint // parameter bindings (narrowing checks)
	Expiry      time.Time    // hard expiry
	Epoch       int          // mint epoch; verified against the broker's min-epoch
}

// payload is the signed body of a token. The exact serialized bytes are what
// the signature covers and what Verify re-reads; field tags are short and
// stable.
type payload struct {
	ID          string       `json:"i,omitempty"`
	Agent       string       `json:"a"`
	Tool        string       `json:"t"`
	Op          string       `json:"o"`
	Constraints []Constraint `json:"c,omitempty"`
	ExpiryNanos int64        `json:"x"`
	Epoch       int          `json:"e"`
}

// envelope wraps the signed payload bytes and the detached Ed25519 signature.
// encoding/json renders []byte as base64, so a serialized token is compact ASCII.
type envelope struct {
	Payload []byte `json:"p"`
	Sig     []byte `json:"s"`
}

// Mint builds a token for g, signed with the broker's private key. The signature
// covers the canonical payload bytes (bound agent, capability, constraints,
// expiry, epoch); Verify checks the signature and then those fields.
func Mint(priv ed25519.PrivateKey, g Grant) ([]byte, error) {
	if g.Agent == "" {
		return nil, fmt.Errorf("token: grant has empty agent")
	}
	if g.Capability.Tool == "" || g.Capability.Operation == "" {
		return nil, fmt.Errorf("token: grant has empty capability")
	}
	for i, c := range g.Constraints {
		if c.Key == "" {
			return nil, fmt.Errorf("token: constraint %d has empty key", i)
		}
	}
	var idb [8]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, fmt.Errorf("token: mint id: %w", err)
	}
	plBytes, err := json.Marshal(payload{
		ID:          hex.EncodeToString(idb[:]),
		Agent:       g.Agent,
		Tool:        g.Capability.Tool,
		Op:          g.Capability.Operation,
		Constraints: g.Constraints,
		ExpiryNanos: g.Expiry.UnixNano(),
		Epoch:       g.Epoch,
	})
	if err != nil {
		return nil, fmt.Errorf("token: marshal payload: %w", err)
	}
	tok, err := json.Marshal(envelope{Payload: plBytes, Sig: ed25519.Sign(priv, plBytes)})
	if err != nil {
		return nil, fmt.Errorf("token: marshal envelope: %w", err)
	}
	return tok, nil
}

// ID returns the token's mint-time id for audit correlation, or "" if the
// token cannot be parsed or the embedded id is not exactly 16 lowercase hex
// chars. It deliberately does NOT verify the signature — audit code calls it
// on deny paths too, where the id is a claimed (attacker-controlled) value;
// the shape check keeps arbitrary bytes out of the audit log.
func ID(tok []byte) string {
	var env envelope
	if err := json.Unmarshal(tok, &env); err != nil {
		return ""
	}
	var pl payload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return ""
	}
	if len(pl.ID) != 16 {
		return ""
	}
	for _, c := range pl.ID {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return pl.ID
}

// Request is the context the broker checks a token against, per exercise.
type Request struct {
	Caller     string            // the mTLS-authenticated caller identity
	Capability Capability        // the (tool, operation) being attempted
	Params     map[string]string // the request's parameters (for constraint checks)
	Now        time.Time         // current time
	MinEpoch   int               // the broker's current minimum acceptable epoch
}

// Verify returns nil iff the signature is valid for pub, caller == bound_agent,
// the requested capability matches, every constraint is satisfied by a present,
// equal request parameter, and the token is unexpired and at or above MinEpoch.
// Fails closed at every gate; param values are compared as opaque data.
func Verify(pub ed25519.PublicKey, tok []byte, r Request) error {
	if r.Now.IsZero() {
		return fmt.Errorf("token: request has zero time")
	}
	if r.Capability.Tool == "" || r.Capability.Operation == "" {
		return fmt.Errorf("token: request has empty capability")
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("token: bad public key")
	}
	var env envelope
	if err := json.Unmarshal(tok, &env); err != nil {
		return fmt.Errorf("token: unmarshal: %w", err)
	}
	if !ed25519.Verify(pub, env.Payload, env.Sig) {
		return fmt.Errorf("token: signature")
	}
	var pl payload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return fmt.Errorf("token: unmarshal payload: %w", err)
	}
	if r.Caller != pl.Agent {
		return fmt.Errorf("token: denied: caller %q != bound_agent %q", r.Caller, pl.Agent)
	}
	if r.Capability.Tool != pl.Tool || r.Capability.Operation != pl.Op {
		return fmt.Errorf("token: denied: capability not granted")
	}
	for _, c := range pl.Constraints {
		v, ok := r.Params[c.Key]
		if !ok || v != c.Value {
			return fmt.Errorf("token: denied: constraint %s=%q not satisfied", c.Key, c.Value)
		}
	}
	if !r.Now.Before(time.Unix(0, pl.ExpiryNanos)) {
		return fmt.Errorf("token: denied: expired")
	}
	if pl.Epoch < r.MinEpoch {
		return fmt.Errorf("token: denied: epoch %d below min %d", pl.Epoch, r.MinEpoch)
	}
	return nil
}
