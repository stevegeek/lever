package agent

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// allowDelegate adds a delegation rule to the broker's policy: agent may delegate
// (tool, op) to recipient.
func allowDelegate(t *testing.T, env *brokerEnv, agent, tool, op, recipient string) {
	t.Helper()
	env.Rules.AllowDelegate(agent, tool, op, recipient)
}

// regDB registers the "db" tool with a "read" operation in the broker's registry.
func regDB(t *testing.T, env *brokerEnv) {
	t.Helper()
	if err := env.Registry.Register(registry.Tool{
		Name:    "db",
		Backend: "http://127.0.0.1:3201",
		Operations: map[string]registry.Operation{
			"read": {Name: "read"},
		},
	}); err != nil {
		t.Fatalf("regDB: %v", err)
	}
}

// enrolManager signs a manager CSR directly with the CA (bypassing /provision
// since the manager identity needs a cert to call /provision in the first place),
// then builds an Identity from the signed cert, key, and CA PEM.
func enrolManager(t *testing.T, caInst *ca.CA) Identity {
	t.Helper()
	csrPEM, keyPEM, err := GenerateCSR("manager")
	if err != nil {
		t.Fatalf("enrolManager: generate CSR: %v", err)
	}
	certPEM, err := caInst.SignCSR(csrPEM)
	if err != nil {
		t.Fatalf("enrolManager: sign CSR: %v", err)
	}
	return Identity{CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: caInst.CertPEM()}
}

// decodeB64 decodes a base64url (no-padding) string; fatals on error.
func decodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decodeB64: %v", err)
	}
	return b
}

// encodeB64 encodes bytes to base64url (no-padding).
func encodeB64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestRequestMintsDelegatedToken(t *testing.T) {
	env := testBroker(t)
	// Allow manager to delegate db.read to worker; register the db tool envelope.
	allowDelegate(t, env, "manager", "db", "read", "worker")
	regDB(t, env)
	managerID := enrolManager(t, env.CA)
	client, err := managerID.Client()
	if err != nil {
		t.Fatal(err)
	}
	tokB64, err := Request(context.Background(), env.Server.URL, client, "db", "read", "worker", map[string]string{"table": "A"})
	if err != nil {
		t.Fatal(err)
	}
	// The minted token must verify as bound to worker for db.read with table=A.
	raw := decodeB64(t, tokB64)
	if err := token.Verify(env.Keys.Public, raw, token.Request{
		Caller: "worker", Capability: token.Capability{Tool: "db", Operation: "read"},
		Params: map[string]string{"table": "A"}, Now: time.Now(), MinEpoch: 0,
	}); err != nil {
		t.Fatalf("minted token must verify for worker/db.read/table=A: %v", err)
	}
}

func TestAttenuateNarrowsOnly(t *testing.T) {
	// Mint a base token (no filter) directly, then attenuate adding filter=alice;
	// the attenuated token must still verify with filter=alice AND fail without it.
	kp, _ := token.Generate()
	raw, _ := token.Mint(kp.Private, token.Grant{
		Agent: "worker", Capability: token.Capability{Tool: "db", Operation: "read"},
		Constraints: []token.Constraint{{Key: "table", Value: "A"}},
		Expiry:      time.Now().Add(time.Hour), Epoch: 0,
	})
	baseB64 := encodeB64(raw)
	narrowed, err := Attenuate(baseB64, map[string]string{"filter": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	nraw := decodeB64(t, narrowed)
	req := func(params map[string]string) error {
		return token.Verify(kp.Public, nraw, token.Request{
			Caller: "worker", Capability: token.Capability{Tool: "db", Operation: "read"},
			Params: params, Now: time.Now(), MinEpoch: 0,
		})
	}
	if err := req(map[string]string{"table": "A", "filter": "alice"}); err != nil {
		t.Fatalf("attenuated token must satisfy with the added constraint: %v", err)
	}
	if req(map[string]string{"table": "A"}) == nil {
		t.Fatal("attenuated token must FAIL without the added filter constraint")
	}
}
