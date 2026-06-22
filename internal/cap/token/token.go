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
	Agent  string    // the agent identity this capability is bound to
	Tools  []string  // the operations the agent may perform (e.g. "qmd.read")
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
	for i, tool := range g.Tools {
		key := fmt.Sprintf("t%d", i)
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
