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
