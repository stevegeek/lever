package token

import (
	"testing"
	"time"
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
