package token

import (
	"encoding/json"
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
	if err := Verify(kp.Public, []byte("not a token"), baseReq()); err == nil {
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

// A constrained param value is matched as opaque data; a value that looks like
// policy syntax must not be interpreted, only compared literally (and so denied
// when it does not equal the minted constraint).
func TestVerifyDeniesInjectionShapedValue(t *testing.T) {
	kp, tok := mintFixture(t)
	r := baseReq()
	r.Params = map[string]string{"table": `A"); allow if true; //`}
	if err := Verify(kp.Public, tok, r); err == nil {
		t.Fatal("expected denial: param value must be inert opaque data, not policy")
	}
}

// A single flipped byte anywhere in the serialized token must invalidate it —
// either the envelope no longer parses or the signature no longer matches.
func TestVerifyDeniesTamperedToken(t *testing.T) {
	kp, tok := mintFixture(t)
	tampered := make([]byte, len(tok))
	copy(tampered, tok)
	tampered[len(tampered)/2] ^= 0xff
	if err := Verify(kp.Public, tampered, baseReq()); err == nil {
		t.Fatal("expected denial: a tampered token must fail verification")
	}
}

func TestVerifyDeniesTruncatedToken(t *testing.T) {
	kp, tok := mintFixture(t)
	if err := Verify(kp.Public, tok[:len(tok)/2], baseReq()); err == nil {
		t.Fatal("expected denial: a truncated token must fail closed")
	}
}

func TestMintMultipleConstraintsAllMustMatch(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Mint(kp.Private, Grant{
		Agent:       "scratch",
		Capability:  Capability{Tool: "db", Operation: "read"},
		Constraints: []Constraint{{Key: "table", Value: "A"}, {Key: "filter", Value: "Y"}},
		Expiry:      time.Now().Add(time.Hour),
		Epoch:       0,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := Request{Caller: "scratch", Capability: Capability{Tool: "db", Operation: "read"}, Now: time.Now(), MinEpoch: 0}
	r := base
	r.Params = map[string]string{"table": "A", "filter": "Y"}
	if err := Verify(kp.Public, tok, r); err != nil {
		t.Fatalf("expected allow with all constraints satisfied: %v", err)
	}
	r2 := base
	r2.Params = map[string]string{"table": "A"}
	if err := Verify(kp.Public, tok, r2); err == nil {
		t.Fatal("expected denial: second constraint filter==Y not satisfied")
	}
}

func TestMintEmbedsUniqueTokenID(t *testing.T) {
	kp, tok := mintFixture(t)
	id := ID(tok)
	if len(id) != 32 {
		t.Fatalf("ID(minted token) = %q, want a 32-char hex token id", id)
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("ID(minted token) = %q, want lowercase hex", id)
		}
	}
	// The ID rides inside the signed payload, so the token must still verify.
	if err := Verify(kp.Public, tok, baseReq()); err != nil {
		t.Fatalf("token with embedded id must verify: %v", err)
	}
	tok2, err := Mint(kp.Private, sampleGrant())
	if err != nil {
		t.Fatal(err)
	}
	if ID(tok2) == id {
		t.Fatalf("two mints produced the same token id %q; ids must be unique per mint", id)
	}
}

func TestIDEmptyForGarbageAndLegacyTokens(t *testing.T) {
	if got := ID([]byte("not a token")); got != "" {
		t.Fatalf("ID(garbage) = %q, want empty", got)
	}
	if got := ID(nil); got != "" {
		t.Fatalf("ID(nil) = %q, want empty", got)
	}
}

func TestIDRejectsMalformedIDShape(t *testing.T) {
	// ID is a best-effort parse for audit lines: on paths where the signature
	// has not (yet) been verified the embedded id is attacker-controlled, so
	// anything that is not exactly 32 lowercase hex chars must come back "".
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	g := sampleGrant()
	tok, err := Mint(kp.Private, g)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the envelope with a junk id in the payload (signature invalid —
	// irrelevant, ID never checks signatures).
	forged := forgeTokenWithID(t, tok, "x\nid=evil decision=allow")
	if got := ID(forged); got != "" {
		t.Fatalf("ID(forged junk id) = %q, want empty (shape-checked)", got)
	}
}

// forgeTokenWithID re-wraps tok's payload with the given id, leaving the (now
// stale) signature in place — ID() must not care.
func forgeTokenWithID(t *testing.T, tok []byte, id string) []byte {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(tok, &env); err != nil {
		t.Fatal(err)
	}
	var pl map[string]any
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		t.Fatal(err)
	}
	pl["i"] = id
	plBytes, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(envelope{Payload: plBytes, Sig: env.Sig})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
