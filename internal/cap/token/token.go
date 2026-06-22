package token

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/parser"
)

// Grant is the capability the broker mints for one agent.
type Grant struct {
	Agent string // the agent identity this capability is bound to
	// Tools lists the operations the agent may perform (e.g. "qmd.read").
	// Matching is EXACT and opaque: no globbing or prefix semantics.
	// Duplicate entries are silently deduplicated.
	Tools  []string
	Expiry time.Time // hard expiry
	Epoch  int       // mint epoch; verified against the broker's current min-epoch
}

// Mint builds a biscuit for g, signed with the broker's private key. The token
// carries the agent, its tool set, the epoch, and three intrinsic checks:
// expiry, epoch floor (revocation), and caller==agent (non-transferability).
func Mint(priv ed25519.PrivateKey, g Grant) ([]byte, error) {
	if g.Agent == "" {
		return nil, fmt.Errorf("token: grant has empty agent")
	}
	var sb strings.Builder
	params := map[string]biscuit.Term{
		"agent":  biscuit.String(g.Agent),
		"expiry": biscuit.Date(g.Expiry),
		"epoch":  biscuit.Integer(int64(g.Epoch)),
	}
	sb.WriteString("agent({agent});\n")
	sb.WriteString("epoch({epoch});\n")
	seen := make(map[string]struct{}, len(g.Tools))
	for _, tool := range g.Tools {
		if _, dup := seen[tool]; dup {
			continue
		}
		seen[tool] = struct{}{}
		key := fmt.Sprintf("t%d", len(seen)-1)
		params[key] = biscuit.String(tool)
		sb.WriteString(fmt.Sprintf("tool({%s});\n", key))
	}
	// Intrinsic checks (always evaluated; reference authorizer-injected facts).
	sb.WriteString("check if time($t), $t < {expiry};\n")
	sb.WriteString("check if epoch($e), min_epoch($m), $e >= $m;\n")
	sb.WriteString("check if caller($c), agent($a), $c == $a;\n")

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

// Request is the context the broker checks a token against, per call.
type Request struct {
	Caller    string    // the mTLS-authenticated caller identity
	Operation string    // the operation being attempted (e.g. "qmd.read")
	Now       time.Time // current time
	MinEpoch  int       // the broker's current minimum acceptable epoch
}

// Verify checks tok against the public key and r. It returns nil iff the
// signature is valid, all intrinsic checks pass (expiry, epoch >= MinEpoch,
// caller == bound agent), and Operation is in the granted tool set.
// Operation must EXACTLY equal one of the token's tools; no globbing or
// prefix matching is performed.
func Verify(pub ed25519.PublicKey, tok []byte, r Request) error {
	if r.Now.IsZero() {
		return fmt.Errorf("token: request has zero time")
	}
	b, err := biscuit.Unmarshal(tok)
	if err != nil {
		return fmt.Errorf("token: unmarshal: %w", err)
	}
	authz, err := b.Authorizer(pub) // verifies the signature against the root public key
	if err != nil {
		return fmt.Errorf("token: signature: %w", err)
	}
	contents, err := parser.FromStringAuthorizerWithParams(
		"caller({caller});\noperation({op});\ntime({now});\nmin_epoch({min});\nallow if operation($o), tool($o);\n",
		map[string]biscuit.Term{
			"caller": biscuit.String(r.Caller),
			"op":     biscuit.String(r.Operation),
			"now":    biscuit.Date(r.Now),
			"min":    biscuit.Integer(int64(r.MinEpoch)),
		},
	)
	if err != nil {
		return fmt.Errorf("token: parse authorizer: %w", err)
	}
	authz.AddAuthorizer(contents)
	if err := authz.Authorize(); err != nil {
		return fmt.Errorf("token: denied: %w", err)
	}
	return nil
}
