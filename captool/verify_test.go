package captool

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
)

// serverBoundTo's backstop forbids table "C"; ran records whether the
// handler ran.
func serverBoundTo(t *testing.T, kp token.KeyPair, ran *bool) *Server {
	t.Helper()
	s := newTestServer(t,
		func(_ ValidatedContext, a map[string]string) error {
			if a["table"] == "C" {
				return errors.New("backstop denied")
			}
			return nil
		},
		func(_ ValidatedContext, a map[string]string) (any, error) { *ran = true; return a, nil })
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

// assertVerifyDenies mints a token for "worker" with cons, calls read as
// caller for table, and requires a JSON-RPC error with the handler never run.
func assertVerifyDenies(t *testing.T, cons map[string]string, tweak func(*Server), caller, table, why string) {
	t.Helper()
	kp, _ := token.Generate()
	var ran bool
	s := serverBoundTo(t, kp, &ran)
	if tweak != nil {
		tweak(s)
	}
	tok := mintTok(t, kp, "worker", cons)
	w := callRead(t, s, caller, table, tok)
	if ran || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("%s; ran=%v body=%s", why, ran, w.Body.String())
	}
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
	assertVerifyDenies(t, map[string]string{"table": "A"}, nil, "", "A", "missing X-Lever-Caller must deny")
}

func TestVerifyDeniesWrongCallerWithoutRunningHandler(t *testing.T) {
	assertVerifyDenies(t, map[string]string{"table": "A"}, nil, "analyst", "A", "bound_agent mismatch must deny")
}

func TestVerifyDeniesConstraintViolationWithoutRunningHandler(t *testing.T) {
	assertVerifyDenies(t, map[string]string{"table": "A"}, nil, "worker", "B", "constraint mismatch must deny")
}

func TestVerifyDeniesBackstopViolationWithoutRunningHandler(t *testing.T) {
	assertVerifyDenies(t, nil, nil, "worker", "C", "backstop must deny table C even with a permissive token")
}

func TestVerifyDeniesStaleEpochWithoutRunningHandler(t *testing.T) {
	// The token was minted at epoch 0; epochTTL=1h prevents a refetch.
	staleEpoch := func(s *Server) { s.epoch = 1 }
	assertVerifyDenies(t, map[string]string{"table": "A"}, staleEpoch, "worker", "A", "stale-epoch token must deny")
}

// TestVerifyDeniesBeforeRegisterWithoutPanic: tools/call before Register has
// set the public key is denied without running the handler or panicking.
func TestVerifyDeniesBeforeRegisterWithoutPanic(t *testing.T) {
	kp, _ := token.Generate()
	var ran bool
	s, err := New(Config{
		Name: "db", Backend: "127.0.0.1:0", AdminURL: "http://127.0.0.1:0", Secret: testToolSecret,
		Operations: []Operation{{
			Name:    "read",
			Handler: func(_ ValidatedContext, _ map[string]string) (any, error) { ran = true; return nil, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tok := mintTok(t, kp, "worker", map[string]string{"table": "A"})
	var w *httptest.ResponseRecorder
	require := func() {
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

// A constraint key that caveat_param renames binds only its argument, as at
// the broker: an argument named like the key is refused before the handler
// runs, and the renamed argument is checked as usual.
func TestVerifyRenamedConstraintKeyBindsOnlyItsArgument(t *testing.T) {
	call := func(args string) (string, bool) {
		kp, _ := token.Generate()
		var ran bool
		s := serverBoundTo(t, kp, &ran)
		o := s.ops["read"]
		o.CaveatParam = map[string]string{"repo": "repository"}
		s.ops["read"] = o
		tok := mintTok(t, kp, "worker", map[string]string{"repo": "allowed"})
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read","arguments":{` + args + `,"_capability":"` + tok + `"}}}`
		w := rpc(t, s, body, map[string]string{"X-Lever-Caller": "worker"})
		return w.Body.String(), ran
	}
	for _, args := range []string{`"repo":"allowed","Repository":"evil"`, `"repo":"allowed"`, `"Repo":"allowed","repository":"allowed"`} {
		if body, ran := call(args); ran || !strings.Contains(body, "invalid params") {
			t.Errorf("SECURITY: %s: ran=%v body=%s", args, ran, body)
		}
	}
	if body, ran := call(`"Repository":"allowed"`); ran || !strings.Contains(body, "forbidden") {
		t.Errorf("SECURITY: the renamed argument under another case must not bind the key: ran=%v body=%s", ran, body)
	}
	if body, ran := call(`"repository":"evil"`); ran || !strings.Contains(body, "forbidden") {
		t.Errorf("a forbidden value: ran=%v body=%s", ran, body)
	}
	if body, ran := call(`"repository":"allowed"`); !ran {
		t.Errorf("the renamed argument must pass: body=%s", body)
	}
}
