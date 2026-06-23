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

func baseReq() Request {
	return Request{
		Caller:     "scratch",
		Capability: Capability{Tool: "db", Operation: "read"},
		Params:     map[string]string{"table": "A"},
		Now:        time.Now(),
		MinEpoch:   0,
	}
}

func TestVerifyDeniesWrongCaller(t *testing.T) {
	kp, tok := mintFixture(t)
	r := baseReq()
	r.Caller = "evil" // presenting scratch's token as someone else
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: caller != bound_agent (cross-agent theft)")
	}
}

func TestVerifyDeniesWrongCapability(t *testing.T) {
	kp, tok := mintFixture(t)
	r := baseReq()
	r.Capability = Capability{Tool: "db", Operation: "write"} // granted read, not write
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: capability not granted")
	}
}

func TestVerifyDeniesWrongTool(t *testing.T) {
	kp, tok := mintFixture(t)
	r := baseReq()
	r.Capability = Capability{Tool: "calendar", Operation: "read"}
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: tool not granted")
	}
}

func TestVerifyDeniesUnsatisfiedConstraint(t *testing.T) {
	kp, tok := mintFixture(t) // constraint table==A
	r := baseReq()
	r.Params = map[string]string{"table": "C"} // wrong value
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: constraint table==A not satisfied by table==C")
	}
}

func TestVerifyDeniesMissingConstrainedParam(t *testing.T) {
	kp, tok := mintFixture(t) // constraint table==A
	r := baseReq()
	r.Params = map[string]string{} // table param absent entirely
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: constrained param absent")
	}
}

func TestVerifyDeniesExpired(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	g := sampleGrant()
	g.Expiry = time.Now().Add(-time.Minute)
	tok, err := Mint(kp.Private, g)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(kp.Public, tok, baseReq()); err == nil {
		t.Fatal("expected denial: token expired")
	}
}

func TestVerifyDeniesStaleEpoch(t *testing.T) {
	kp, tok := mintFixture(t) // minted at epoch 0
	r := baseReq()
	r.MinEpoch = 1 // broker raised the floor -> revoked
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: token epoch below min epoch (revoked)")
	}
}

func TestVerifyDeniesWrongKey(t *testing.T) {
	_, tok := mintFixture(t)
	other, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(other.Public, tok, baseReq()); err == nil {
		t.Fatal("expected denial: token not signed by this key")
	}
}

func TestVerifyDeniesGarbage(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(kp.Public, []byte("not a biscuit"), baseReq()); err == nil {
		t.Fatal("expected denial: unparseable token must fail closed")
	}
}

func TestVerifyDeniesZeroTime(t *testing.T) {
	kp, tok := mintFixture(t)
	r := baseReq()
	r.Now = time.Time{}
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: zero request time must fail closed")
	}
}

func TestVerifyDeniesInjectionShapedValue(t *testing.T) {
	kp, tok := mintFixture(t)
	r := baseReq()
	r.Params = map[string]string{"table": `A"); allow if true; //`}
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: param value must be inert opaque data, not Datalog")
	}
}
