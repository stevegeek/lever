package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/parser"
)

// Capability is the single (tool, operation) a token authorizes.
type Capability struct {
	Tool      string
	Operation string
}

// Constraint binds a request parameter to a fixed value. Each constraint
// becomes a `check if param(key, value)` in the token, so a request is allowed
// only if it carries that exact parameter. Constraints can only narrow.
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

// Mint builds a biscuit for g, signed with the broker's private key. The token
// carries bound_agent, the capability, the epoch, one check per constraint, and
// three intrinsic checks: expiry, epoch floor (revocation), caller==bound_agent.
func Mint(priv ed25519.PrivateKey, g Grant) ([]byte, error) {
	if g.Agent == "" {
		return nil, fmt.Errorf("token: grant has empty agent")
	}
	if g.Capability.Tool == "" || g.Capability.Operation == "" {
		return nil, fmt.Errorf("token: grant has empty capability")
	}
	var sb strings.Builder
	params := map[string]biscuit.Term{
		"agent":  biscuit.String(g.Agent),
		"tool":   biscuit.String(g.Capability.Tool),
		"op":     biscuit.String(g.Capability.Operation),
		"expiry": biscuit.Date(g.Expiry),
		"epoch":  biscuit.Integer(int64(g.Epoch)),
	}
	sb.WriteString("bound_agent({agent});\n")
	sb.WriteString("capability({tool}, {op});\n")
	sb.WriteString("epoch({epoch});\n")
	for i, c := range g.Constraints {
		if c.Key == "" {
			return nil, fmt.Errorf("token: constraint %d has empty key", i)
		}
		kk := fmt.Sprintf("ck%d", i)
		vk := fmt.Sprintf("cv%d", i)
		params[kk] = biscuit.String(c.Key)
		params[vk] = biscuit.String(c.Value)
		sb.WriteString(fmt.Sprintf("check if param({%s}, {%s});\n", kk, vk))
	}
	// Intrinsic checks (reference authorizer-injected facts).
	sb.WriteString("check if time($t), $t < {expiry};\n")
	sb.WriteString("check if epoch($e), min_epoch($m), $e >= $m;\n")
	sb.WriteString("check if caller($c), bound_agent($a), $c == $a;\n")

	block, err := parser.FromStringBlockWithParams(sb.String(), params)
	if err != nil {
		return nil, fmt.Errorf("token: parse block: %w", err)
	}
	b := biscuit.NewBuilder(priv)
	if err := b.AddBlock(block); err != nil {
		return nil, fmt.Errorf("token: add block: %w", err)
	}
	built, err := b.Build()
	if err != nil {
		return nil, fmt.Errorf("token: build: %w", err)
	}
	serialized, err := built.Serialize()
	if err != nil {
		return nil, fmt.Errorf("token: serialize: %w", err)
	}
	return serialized, nil
}

// Request is the context the broker checks a token against, per exercise.
type Request struct {
	Caller     string            // the mTLS-authenticated caller identity
	Capability Capability        // the (tool, operation) being attempted
	Params     map[string]string // the request's parameters (for constraint checks)
	Now        time.Time         // current time
	MinEpoch   int               // the broker's current minimum acceptable epoch
}

// Verify checks tok against the public key and r. Returns nil iff the signature
// is valid, caller == bound_agent, the requested capability matches, every
// constraint check is satisfied by r.Params, and the token is unexpired and at
// or above MinEpoch. Param values are injected as opaque facts (no injection).
func Verify(pub ed25519.PublicKey, tok []byte, r Request) error {
	if r.Now.IsZero() {
		return fmt.Errorf("token: request has zero time")
	}
	if r.Capability.Tool == "" || r.Capability.Operation == "" {
		return fmt.Errorf("token: request has empty capability")
	}
	b, err := biscuit.Unmarshal(tok)
	if err != nil {
		return fmt.Errorf("token: unmarshal: %w", err)
	}
	authz, err := b.Authorizer(pub) // verifies the signature against the root public key
	if err != nil {
		return fmt.Errorf("token: signature: %w", err)
	}

	var sb strings.Builder
	params := map[string]biscuit.Term{
		"caller": biscuit.String(r.Caller),
		"tool":   biscuit.String(r.Capability.Tool),
		"op":     biscuit.String(r.Capability.Operation),
		"now":    biscuit.Date(r.Now),
		"min":    biscuit.Integer(int64(r.MinEpoch)),
	}
	sb.WriteString("caller({caller});\n")
	sb.WriteString("request_tool({tool});\n")
	sb.WriteString("request_op({op});\n")
	sb.WriteString("time({now});\n")
	sb.WriteString("min_epoch({min});\n")
	// Each param fact is self-contained (key and value travel together), so map
	// iteration order is cosmetic; the pk%d/pv%d indices only name placeholders.
	i := 0
	for k, v := range r.Params {
		kk := fmt.Sprintf("pk%d", i)
		vk := fmt.Sprintf("pv%d", i)
		params[kk] = biscuit.String(k)
		params[vk] = biscuit.String(v)
		sb.WriteString(fmt.Sprintf("param({%s}, {%s});\n", kk, vk))
		i++
	}
	sb.WriteString("allow if capability($t, $o), request_tool($t), request_op($o);\n")

	contents, err := parser.FromStringAuthorizerWithParams(sb.String(), params)
	if err != nil {
		return fmt.Errorf("token: parse authorizer: %w", err)
	}
	authz.AddAuthorizer(contents)
	if err := authz.Authorize(); err != nil {
		return fmt.Errorf("token: denied: %w", err)
	}
	return nil
}

// Attenuate appends a block adding a narrowing check per extra constraint. It
// needs only the token (not the root key); appended checks can only restrict,
// never widen. Origin-scoping guarantees an appended fact cannot satisfy the
// authority-block authorizer policy.
func Attenuate(tok []byte, extra []Constraint) ([]byte, error) {
	b, err := biscuit.Unmarshal(tok)
	if err != nil {
		return nil, fmt.Errorf("token: unmarshal: %w", err)
	}
	bb := b.CreateBlock()
	var sb strings.Builder
	params := map[string]biscuit.Term{}
	for i, c := range extra {
		if c.Key == "" {
			return nil, fmt.Errorf("token: constraint %d has empty key", i)
		}
		kk := fmt.Sprintf("ak%d", i)
		vk := fmt.Sprintf("av%d", i)
		params[kk] = biscuit.String(c.Key)
		params[vk] = biscuit.String(c.Value)
		sb.WriteString(fmt.Sprintf("check if param({%s}, {%s});\n", kk, vk))
	}
	block, err := parser.FromStringBlockWithParams(sb.String(), params)
	if err != nil {
		return nil, fmt.Errorf("token: parse attenuation block: %w", err)
	}
	if err := bb.AddBlock(block); err != nil {
		return nil, fmt.Errorf("token: add attenuation block: %w", err)
	}
	appended, err := b.Append(rand.Reader, bb.Build())
	if err != nil {
		return nil, fmt.Errorf("token: append: %w", err)
	}
	out, err := appended.Serialize()
	if err != nil {
		return nil, fmt.Errorf("token: serialize: %w", err)
	}
	return out, nil
}
