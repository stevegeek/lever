package captool

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
)

// serverBoundTo builds a captool Server whose pubKey is kp.Public and records
// whether the operation handler ran. The backstop forbids table "C".
func serverBoundTo(t *testing.T, kp token.KeyPair, ran *bool) *Server {
	t.Helper()
	s, err := New(Config{
		Name: "db", Backend: "127.0.0.1:0", AdminURL: "http://127.0.0.1:0",
		Operations: []Operation{{
			Name: "read", CaveatParam: map[string]string{"table": "table", "filter": "filter"},
			Backstop: func(_ ValidatedContext, a map[string]string) error {
				if a["table"] == "C" {
					return ErrBackstop
				}
				return nil
			},
			Handler: func(_ ValidatedContext, a map[string]string) (any, error) { *ran = true; return a, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.pubKey = kp.Public
	s.epoch, s.epochAt, s.epochTTL = 0, time.Now(), time.Hour
	return s
}

func mintTok(t *testing.T, kp token.KeyPair, agent string, cons map[string]string) string {
	t.Helper()
	c := make([]token.Constraint, 0, len(cons))
	for k, v := range cons {
		c = append(c, token.Constraint{Key: k, Value: v})
	}
	tok, err := token.Mint(kp.Private, token.Grant{
		Agent: agent, Capability: token.Capability{Tool: "db", Operation: "read"},
		Constraints: c, Expiry: time.Now().Add(time.Hour), Epoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(tok)
}

func callRead(t *testing.T, s *Server, caller, table, capB64 string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{"table":"` + table + `","_capability":"` + capB64 + `"}}}`
	h := map[string]string{}
	if caller != "" {
		h["X-Lever-Caller"] = caller
	}
	return rpc(t, s, body, h)
}

func TestVerifyAllowsValidCallAndRunsHandler(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	s := serverBoundTo(t, kp, &ran)
	tok := mintTok(t, kp, "worker", map[string]string{"table": "A"})
	w := callRead(t, s, "worker", "A", tok)
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, isErr := resp["error"]; isErr || !ran {
		t.Fatalf("valid call must run handler; body=%s ran=%v", w.Body.String(), ran)
	}
}

func TestVerifyDeniesMissingCallerWithoutRunningHandler(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	s := serverBoundTo(t, kp, &ran)
	tok := mintTok(t, kp, "worker", map[string]string{"table": "A"})
	w := callRead(t, s, "", "A", tok)
	if ran || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("missing X-Lever-Caller must deny; ran=%v body=%s", ran, w.Body.String())
	}
}

func TestVerifyDeniesWrongCallerWithoutRunningHandler(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	s := serverBoundTo(t, kp, &ran)
	tok := mintTok(t, kp, "worker", map[string]string{"table": "A"}) // bound to worker
	w := callRead(t, s, "analyst", "A", tok)                         // caller analyst
	if ran || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("bound_agent mismatch must deny; ran=%v", ran)
	}
}

func TestVerifyDeniesConstraintViolationWithoutRunningHandler(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	s := serverBoundTo(t, kp, &ran)
	tok := mintTok(t, kp, "worker", map[string]string{"table": "A"}) // constrained to A
	w := callRead(t, s, "worker", "B", tok)                          // requests B
	if ran || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("constraint mismatch must deny; ran=%v", ran)
	}
}

func TestVerifyDeniesBackstopViolationWithoutRunningHandler(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	s := serverBoundTo(t, kp, &ran)
	// Token permits table C (no table constraint), but the backstop forbids C.
	tok := mintTok(t, kp, "worker", nil)
	w := callRead(t, s, "worker", "C", tok)
	if ran || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("backstop must deny table C even with a permissive token; ran=%v", ran)
	}
}

func TestVerifyDeniesStaleEpochWithoutRunningHandler(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	s := serverBoundTo(t, kp, &ran)
	s.epoch = 1 // broker has moved to epoch 1; token was minted at 0 (epochTTL=1h so no refetch)
	tok := mintTok(t, kp, "worker", map[string]string{"table": "A"})
	w := callRead(t, s, "worker", "A", tok)
	if ran || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("stale-epoch token must deny; ran=%v", ran)
	}
}

// TestVerifyDeniesBeforeRegisterWithoutPanic ensures that serving tools/call
// before Register() has populated the public key returns a JSON-RPC error,
// does NOT run the handler, and does NOT panic (previously token.Verify would
// panic on nil key before the explicit guard was added).
func TestVerifyDeniesBeforeRegisterWithoutPanic(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	// Build a server the same way as other tests but do NOT set pubKey —
	// simulating tools/call arriving before Register() completes.
	s, err := New(Config{
		Name: "db", Backend: "127.0.0.1:0", AdminURL: "http://127.0.0.1:0",
		Operations: []Operation{{
			Name:    "read",
			Handler: func(_ ValidatedContext, _ map[string]string) (any, error) { ran = true; return nil, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// pubKey is nil — Register never called.
	tok := mintTok(t, kp, "worker", map[string]string{"table": "A"})
	var w *httptest.ResponseRecorder
	require := func() {
		// Wrap in a recover so a panic is caught and turned into a test failure
		// rather than a goroutine crash — we want an explicit message.
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("handleToolsCall panicked with nil pubKey: %v", r)
			}
		}()
		w = callRead(t, s, "worker", "A", tok)
	}
	require()
	if ran {
		t.Fatal("handler must not run when pubKey is nil")
	}
	if !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("must return a JSON-RPC error when pubKey is nil; body=%s", w.Body.String())
	}
}
