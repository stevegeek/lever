package token

import (
	"crypto/ed25519"
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
