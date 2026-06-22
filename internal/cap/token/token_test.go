package token

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/biscuit-auth/biscuit-go/v2"
	"github.com/biscuit-auth/biscuit-go/v2/parser"
)

func TestMintProducesSerializedToken(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	g := Grant{
		Agent:  "scratch",
		Tools:  []string{"qmd.read", "qmd.write"},
		Expiry: time.Now().Add(time.Hour),
		Epoch:  0,
	}
	tok, err := Mint(kp.Private, g)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(tok) == 0 {
		t.Fatal("mint returned empty token")
	}
}

func TestMintRejectsEmptyAgent(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Mint(kp.Private, Grant{Tools: []string{"qmd.read"}, Expiry: time.Now().Add(time.Hour)})
	if err == nil {
		t.Fatal("expected error for empty agent")
	}
}

func mintFixture(t *testing.T) (KeyPair, []byte) {
	t.Helper()
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Mint(kp.Private, Grant{
		Agent:  "scratch",
		Tools:  []string{"qmd.read", "qmd.write"},
		Expiry: time.Now().Add(time.Hour),
		Epoch:  0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return kp, tok
}

func TestVerifyAllowsBoundAgentAndGrantedTool(t *testing.T) {
	kp, tok := mintFixture(t)
	err := Verify(kp.Public, tok, Request{
		Caller:    "scratch",
		Operation: "qmd.read",
		Now:       time.Now(),
		MinEpoch:  0,
	})
	if err != nil {
		t.Fatalf("expected allow, got: %v", err)
	}
}

func TestVerifyDeniesWrongCaller(t *testing.T) {
	kp, tok := mintFixture(t)
	// agent B (caller "evil") presenting agent "scratch"'s token
	err := Verify(kp.Public, tok, Request{Caller: "evil", Operation: "qmd.read", Now: time.Now(), MinEpoch: 0})
	if err == nil {
		t.Fatal("expected denial: caller != bound agent (cross-agent theft)")
	}
}

func TestVerifyDeniesUngrantedTool(t *testing.T) {
	kp, tok := mintFixture(t)
	err := Verify(kp.Public, tok, Request{Caller: "scratch", Operation: "calendar.write", Now: time.Now(), MinEpoch: 0})
	if err == nil {
		t.Fatal("expected denial: operation not in granted tool set")
	}
}

func TestVerifyDeniesExpired(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Mint(kp.Private, Grant{Agent: "scratch", Tools: []string{"qmd.read"}, Expiry: time.Now().Add(-time.Minute), Epoch: 0})
	if err != nil {
		t.Fatal(err)
	}
	err = Verify(kp.Public, tok, Request{Caller: "scratch", Operation: "qmd.read", Now: time.Now(), MinEpoch: 0})
	if err == nil {
		t.Fatal("expected denial: token expired")
	}
}

func TestVerifyDeniesStaleEpoch(t *testing.T) {
	kp, tok := mintFixture(t) // minted at epoch 0
	// broker raised min epoch to 1 -> token is revoked
	err := Verify(kp.Public, tok, Request{Caller: "scratch", Operation: "qmd.read", Now: time.Now(), MinEpoch: 1})
	if err == nil {
		t.Fatal("expected denial: token epoch below broker min epoch (revoked)")
	}
}

func TestVerifyDeniesWrongKey(t *testing.T) {
	_, tok := mintFixture(t)
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = Verify(other.Public, tok, Request{Caller: "scratch", Operation: "qmd.read", Now: time.Now(), MinEpoch: 0})
	if err == nil {
		t.Fatal("expected denial: token not signed by this key")
	}
}

func TestVerifyDeniesGarbage(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	err = Verify(kp.Public, []byte("not a biscuit"), Request{Caller: "scratch", Operation: "qmd.read", Now: time.Now(), MinEpoch: 0})
	if err == nil {
		t.Fatal("expected denial: unparseable token must fail closed")
	}
}

func TestMintAcceptsDuplicateTools(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Mint(kp.Private, Grant{Agent: "scratch", Tools: []string{"qmd.read", "qmd.read"}, Expiry: time.Now().Add(time.Hour), Epoch: 0})
	if err != nil {
		t.Fatalf("mint with duplicate tools should succeed: %v", err)
	}
	if err := Verify(kp.Public, tok, Request{Caller: "scratch", Operation: "qmd.read", Now: time.Now(), MinEpoch: 0}); err != nil {
		t.Fatalf("deduped tool should still be usable: %v", err)
	}
}

func TestVerifyDeniesEmptyToolSet(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Mint(kp.Private, Grant{Agent: "scratch", Tools: nil, Expiry: time.Now().Add(time.Hour), Epoch: 0})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(kp.Public, tok, Request{Caller: "scratch", Operation: "qmd.read", Now: time.Now(), MinEpoch: 0}); err == nil {
		t.Fatal("expected denial: a token with no tools must authorize nothing")
	}
}

func TestVerifyDeniesInjectionShapedOperation(t *testing.T) {
	kp, tok := mintFixture(t)
	evil := `qmd.read"); allow if true; //`
	if err := Verify(kp.Public, tok, Request{Caller: "scratch", Operation: evil, Now: time.Now(), MinEpoch: 0}); err == nil {
		t.Fatal("expected denial: operation string must be inert opaque data, not Datalog")
	}
}

func TestVerifyDeniesZeroTime(t *testing.T) {
	kp, tok := mintFixture(t)
	var zero time.Time
	if err := Verify(kp.Public, tok, Request{Caller: "scratch", Operation: "qmd.read", Now: zero, MinEpoch: 0}); err == nil {
		t.Fatal("expected denial: zero request time must fail closed")
	}
}

// TestVerifyDeniesAppendedAttenuationCannotWiden is the most important property:
// an attacker who appends a block adding a tool must NOT gain that capability.
func TestVerifyDeniesAppendedAttenuationCannotWiden(t *testing.T) {
	kp, tok := mintFixture(t) // tools: qmd.read, qmd.write
	b, err := biscuit.Unmarshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	bb := b.CreateBlock()
	blk, err := parser.FromStringBlockWithParams(`tool({t});`, map[string]biscuit.Term{"t": biscuit.String("calendar.write")})
	if err != nil {
		t.Fatal(err)
	}
	if err := bb.AddBlock(blk); err != nil {
		t.Fatal(err)
	}
	widened, err := b.Append(rand.Reader, bb.Build())
	if err != nil {
		t.Fatal(err)
	}
	ser, err := widened.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(kp.Public, ser, Request{Caller: "scratch", Operation: "calendar.write", Now: time.Now(), MinEpoch: 0}); err == nil {
		t.Fatal("expected denial: an appended block must not widen the granted tool set")
	}
}
