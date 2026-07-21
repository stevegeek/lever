package broker

// directive_e2e_test.go is the end-to-end adversarial validation of the
// operator-directive feature: ONE broker wired to BOTH real channels at once
// — the 0600 UDS admin channel (operator side) and the mTLS jail server
// (agent side) — with real ssh-keygen signing and two distinct agent
// identities. Where the per-route unit tests each prove one property in
// isolation, this test drives the whole attack narrative through the real
// HTTP surfaces in one place, so a regression that only shows up when the
// channels are combined cannot hide.
//
// Target agent = "manager"; non-target = "worker" (a declared worker whose
// mTLS cert the broker accepts but who is never a directive's target). This
// mirrors the live scenario (manager is the hardened directive target; a
// worker is the second agent that must not be able to consume).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
)

func TestOperatorDirectiveEndToEndAdversarial(t *testing.T) {
	b, priv, allowedSigners, rt := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b) // operator -> broker (UDS 0600)
	admin := directiveClient(sock)
	jail := jailServer(t, b) // agent -> broker (mTLS)
	defer jail.Close()

	targetClient := agentClient(t, b, signedCert(t, b, "manager")) // the directive's target
	otherClient := agentClient(t, b, signedCert(t, b, "worker"))   // a second, non-target agent

	// Simulate both agents having enrolled: enrolment is what starts a
	// directive generation, and the send handler binds to the current one.
	b.Directives().BumpGeneration("manager") // -> gen 1
	b.Directives().BumpGeneration("worker")  // -> gen 1

	// consume posts an id to the mTLS /directive/consume as the given agent.
	consume := func(client *http.Client, id string) (int, []byte) {
		t.Helper()
		return postDirectiveID(t, client, jail.URL, "/directive/consume", id)
	}
	// assertOpaque404 consumes as client and requires the byte-identical
	// opaque miss (no existence/target/state oracle).
	assertOpaque404 := func(what string, client *http.Client, id string) {
		t.Helper()
		status, body := consume(client, id)
		if status != http.StatusNotFound || string(body) != opaque404Body {
			t.Fatalf("%s: got %d %q, want 404 %q (opaque, no oracle)", what, status, body, opaque404Body)
		}
	}

	toolAction := toolCallAction("db", "read", `{"table":"secrets"}`)

	// ---- Phase 1: happy path — sign, deliver, consume the exact action ----
	id1 := "e2e00000-0000-4000-8000-000000000001"
	code, body := postSend(t, admin, priv, directiveStatement(id1, "manager", 1, toolAction))
	if code != http.StatusOK {
		t.Fatalf("phase1 send: %d %s", code, body)
	}
	if n := len(rt.messages); n != 1 {
		t.Fatalf("phase1: delivery Message calls = %d, want 1", n)
	}
	if pointer := rt.messages[0].Body; !bytes.Contains([]byte(pointer), []byte(id1)) ||
		bytes.Contains([]byte(pointer), []byte("secrets")) {
		t.Fatalf("phase1: notification must carry the id as a pointer and NOT the action content: %q", pointer)
	}

	// ---- Phase 2: PROPERTY 3 — the non-target cannot consume ----
	// "worker" presents a valid mTLS identity and tries the manager's id,
	// BEFORE the target has consumed it (so it is genuinely still active).
	assertOpaque404("non-target consume (active directive)", otherClient, id1)

	// ---- Phase 3: PROPERTY 1 (mechanism) — the target consumes the exact,
	// signed action; nothing acted-on came from anywhere but the statement ----
	code, body = consume(targetClient, id1)
	if code != http.StatusOK {
		t.Fatalf("phase3 target consume: %d %s", code, body)
	}
	var got struct {
		ID     string       `json:"id"`
		Kind   string       `json:"kind"`
		Action opsig.Action `json:"action"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("phase3 decode: %v (%s)", err, body)
	}
	if got.ID != id1 || got.Kind != "tool_call" || got.Action.Tool != "db" || got.Action.Op != "read" {
		t.Fatalf("phase3: consumed action = %+v, want the signed db/read tool_call", got)
	}

	// ---- Phase 4: replay — a consumed id is inert on both channels ----
	assertOpaque404("replay consume by target", targetClient, id1)
	assertOpaque404("replay consume by non-target", otherClient, id1)
	if code, _ := postSend(t, admin, priv, directiveStatement(id1, "manager", 1, toolAction)); code != http.StatusConflict {
		t.Fatalf("phase4: resubmit of consumed id = %d, want 409 (tombstone)", code)
	}

	// ---- Phase 5: PROPERTY 2 — no path lets injected/forged input act ----
	before := len(b.Directives().List(time.Now()))

	// (a) An attacker who can reach the admin channel but lacks the operator
	// key: sign with a different key that is NOT in allowed_signers.
	attackerPriv, _ := genOperatorKey(t)
	idA := "e2e00000-0000-4000-8000-00000000000a"
	if code, _ := postSend(t, admin, attackerPriv, directiveStatement(idA, "manager", 1, toolAction)); code != http.StatusBadRequest {
		t.Fatalf("phase5a: wrong-key send = %d, want 400", code)
	}

	// (b) Valid signature, but over DIFFERENT bytes than submitted (payload
	// doctored after signing).
	idB := "e2e00000-0000-4000-8000-00000000000b"
	raw, _ := json.Marshal(directiveStatement(idB, "manager", 1, toolAction))
	sig, err := opsig.Sign(priv, opsig.NamespaceDirective, raw)
	if err != nil {
		t.Fatal(err)
	}
	doctored := bytes.Replace(raw, []byte(`"secrets"`), []byte(`"exfil"`), 1)
	if code, _ := postSignedWithSig(t, admin, "/directive/send", doctored, sig); code != http.StatusBadRequest {
		t.Fatalf("phase5b: doctored-payload send = %d, want 400", code)
	}

	// (c) A forged pointer: an injected "directive <id> pending" naming an id
	// that was never signed. The agent that acts on it gets nothing — a
	// notification cannot conjure a directive into the store.
	assertOpaque404("forged-pointer consume", targetClient, "e2e00000-0000-4000-8000-0000deadbeef")

	// (d) Cross-instance: a validly-signed statement for a different instance.
	idD := "e2e00000-0000-4000-8000-00000000000d"
	stD := directiveStatement(idD, "manager", 1, toolAction)
	stD.Instance = "some-other-instance"
	if code, _ := postSend(t, admin, priv, stD); code != http.StatusBadRequest {
		t.Fatalf("phase5d: cross-instance send = %d, want 400", code)
	}

	if after := len(b.Directives().List(time.Now())); after != before {
		t.Fatalf("phase5: store changed under attack (%d -> %d); nothing forged/injected may persist", before, after)
	}

	// ---- Phase 6: recycled-slug — re-enrolment bumps the generation, so a
	// directive for the old generation can never reach the new occupant ----
	b.Directives().BumpGeneration("manager") // -> gen 2 (manager "re-enrolled")
	idOld := "e2e00000-0000-4000-8000-000000000060"
	if code, _ := postSend(t, admin, priv, directiveStatement(idOld, "manager", 1, toolAction)); code != http.StatusConflict {
		t.Fatalf("phase6: stale-generation send = %d, want 409", code)
	}

	// ---- Phase 7: revocation — both agent-side (revoked caller) and
	// operator-side (allowed_signers edit, no restart) ----
	id7 := "e2e00000-0000-4000-8000-000000000070"
	if code, body := postSend(t, admin, priv, directiveStatement(id7, "manager", 2, toolAction)); code != http.StatusOK {
		t.Fatalf("phase7 send (gen 2): %d %s", code, body)
	}
	b.Revoke("manager")
	if code, _ := consume(targetClient, id7); code != http.StatusForbidden {
		t.Fatalf("phase7: revoked caller consume = %d, want 403", code)
	}

	// Live operator-key revocation: truncate allowed_signers on disk — the
	// verifier shells out per call, so the next send fails with no restart.
	if err := os.WriteFile(allowedSigners, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	id8 := "e2e00000-0000-4000-8000-000000000080"
	if code, _ := postSend(t, admin, priv, directiveStatement(id8, "manager", 2, toolAction)); code != http.StatusBadRequest {
		t.Fatalf("phase7: send after allowed_signers cleared = %d, want 400 (live key revocation)", code)
	}
}
