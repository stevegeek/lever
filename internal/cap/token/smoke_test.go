package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/parser"
)

// TestBiscuitGoRoundTrip pins the upstream API: mint -> serialize -> unmarshal
// -> authorize with public key. If this breaks, reconcile the wrapper below.
func TestBiscuitGoRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	block, err := parser.FromStringBlockWithParams(
		`agent({a}); tool({t});`,
		map[string]biscuit.Term{"a": biscuit.String("scratch"), "t": biscuit.String("qmd.read")},
	)
	if err != nil {
		t.Fatalf("parse block: %v", err)
	}
	b := biscuit.NewBuilder(priv)
	if err := b.AddBlock(block); err != nil {
		t.Fatalf("add block: %v", err)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	serialized, err := tok.Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}

	parsed, err := biscuit.Unmarshal(serialized)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	authz, err := parsed.Authorizer(pub)
	if err != nil {
		t.Fatalf("authorizer (signature check): %v", err)
	}
	contents, err := parser.FromStringAuthorizerWithParams(
		`operation({op}); allow if operation($o), tool($o);`,
		map[string]biscuit.Term{"op": biscuit.String("qmd.read")},
	)
	if err != nil {
		t.Fatalf("parse authorizer: %v", err)
	}
	authz.AddAuthorizer(contents)
	if err := authz.Authorize(); err != nil {
		t.Fatalf("expected allow, got: %v", err)
	}
}
