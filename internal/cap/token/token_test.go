package token

import (
	"testing"
	"time"
)

func sampleGrant() Grant {
	return Grant{
		Agent:       "scratch",
		Capability:  Capability{Tool: "db", Operation: "read"},
		Constraints: []Constraint{{Key: "table", Value: "A"}},
		Expiry:      time.Now().Add(time.Hour),
		Epoch:       0,
	}
}

func TestMintProducesSerializedToken(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Mint(kp.Private, sampleGrant())
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
	g := sampleGrant()
	g.Agent = ""
	if _, err := Mint(kp.Private, g); err == nil {
		t.Fatal("expected error for empty agent")
	}
}

func TestMintRejectsEmptyCapability(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	g := sampleGrant()
	g.Capability = Capability{}
	if _, err := Mint(kp.Private, g); err == nil {
		t.Fatal("expected error for empty capability")
	}
}

func mintFixture(t *testing.T) (KeyPair, []byte) {
	t.Helper()
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Mint(kp.Private, sampleGrant())
	if err != nil {
		t.Fatal(err)
	}
	return kp, tok
}

func TestVerifyAllowsBoundAgentCapabilityAndConstraint(t *testing.T) {
	kp, tok := mintFixture(t)
	err := Verify(kp.Public, tok, Request{
		Caller:     "scratch",
		Capability: Capability{Tool: "db", Operation: "read"},
		Params:     map[string]string{"table": "A"},
		Now:        time.Now(),
		MinEpoch:   0,
	})
	if err != nil {
		t.Fatalf("expected allow, got: %v", err)
	}
}
