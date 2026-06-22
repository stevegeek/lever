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
