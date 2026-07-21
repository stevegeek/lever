package broker

// directive_agent_test.go exercises the agent-facing /directive/consume and
// /directive/check mTLS jail routes: happy-path consume, the opaque-404
// failure surface (unknown id, wrong target, replay, expiry, stale
// generation — all byte-identical), revoked-caller and certless denial, and
// the per-CN rate limit. Directives are seeded directly via the store
// (b.Directives().Submit), bypassing the signed /directive/send admin
// channel, per Task 5's brief — these routes never verify signatures
// themselves.

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
)

// toolCallAction builds a bound tool_call action satisfying opsig's
// validateAction (arg_binding "exact", uses 1).
func toolCallAction(tool, op, args string) opsig.Action {
	return opsig.Action{
		Kind: "tool_call", Tool: tool, Op: op,
		Args: json.RawMessage(args), ArgBinding: "exact", Uses: 1,
	}
}

// submitDirective stores st as an active directive directly through the
// store (Submit), bypassing the signed admin channel. Returns the exact
// marshaled statement bytes so callers can compare against the consumed
// action.
func submitDirective(t *testing.T, b *Broker, st opsig.Statement) []byte {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	nbf, err := time.Parse(time.RFC3339, st.NotBefore)
	if err != nil {
		t.Fatal(err)
	}
	exp, err := time.Parse(time.RFC3339, st.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	rec := DirectiveRecord{
		ID: st.DirectiveID, Statement: raw,
		TargetCN: st.TargetAgent.CN, TargetGen: st.TargetAgent.Generation,
		Kind: st.Action.Kind, NotBefore: nbf, ExpiresAt: exp,
	}
	if err := b.Directives().Submit(rec, time.Now()); err != nil {
		t.Fatalf("submit directive %q: %v", st.DirectiveID, err)
	}
	return raw
}

// expiredStatement builds a statement whose expiry is already in the past —
// Submit doesn't itself validate NotBefore/ExpiresAt (only opsig.ParseStatement
// does, and these routes never call it), so this is valid input to Submit but
// must be rejected by Consume/Check's own time check.
func expiredStatement(id, cn string, gen int, action opsig.Action) opsig.Statement {
	now := time.Now()
	return opsig.Statement{
		V: 1, Instance: "testinst", DirectiveID: id,
		TargetAgent: opsig.Target{CN: cn, Generation: gen},
		IssuedAt:    now.Add(-2 * time.Hour).Format(time.RFC3339),
		NotBefore:   now.Add(-2 * time.Hour).Format(time.RFC3339),
		ExpiresAt:   now.Add(-time.Hour).Format(time.RFC3339),
		Action:      action,
	}
}

// postDirectiveID POSTs {"id": id} to path on url and returns the response
// status and raw body bytes. Deliberately builds the request body as an
// inline map rather than referencing the not-yet-implemented
// directiveIDRequest type, so this test file compiles and RUNS against the
// unregistered-route mux during the RED phase.
func postDirectiveID(t *testing.T, client *http.Client, url, path, id string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"id": id})
	resp, err := client.Post(url+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, got
}

const opaque404Body = `{"error":"not found"}`

// wantDirectiveRateLimit mirrors directive_agent.go's directiveRateLimit
// (30 calls/CN/minute). Declared independently — not a reference to that
// constant — so this test file compiles and runs against the
// unregistered-route mux during the RED phase, before directive_agent.go exists.
const wantDirectiveRateLimit = 30

func TestConsumeHappyPathToolCall(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	b.Directives().BumpGeneration("manager") // generation 0 -> 1

	id := "11111111-2222-4333-8444-555555555601"
	action := toolCallAction("db", "read", `{"table":"A"}`)
	st := directiveStatement(id, "manager", 1, action)
	submitDirective(t, b, st)

	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
	if status != http.StatusOK {
		t.Fatalf("consume status = %d, body = %s", status, body)
	}
	var resp struct {
		ID     string       `json:"id"`
		Kind   string       `json:"kind"`
		Action opsig.Action `json:"action"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	if resp.ID != id || resp.Kind != "tool_call" {
		t.Fatalf("response = %+v, want id=%s kind=tool_call", resp, id)
	}
	if resp.Action.Tool != "db" || resp.Action.Op != "read" {
		t.Fatalf("action tool/op = %q/%q, want db/read", resp.Action.Tool, resp.Action.Op)
	}
	if resp.Action.ArgBinding != "exact" || resp.Action.Uses != 1 {
		t.Fatalf("action arg_binding/uses = %q/%d, want exact/1", resp.Action.ArgBinding, resp.Action.Uses)
	}
	var gotArgs, wantArgs map[string]any
	if err := json.Unmarshal(resp.Action.Args, &gotArgs); err != nil {
		t.Fatalf("decode action.args: %v", err)
	}
	if err := json.Unmarshal(action.Args, &wantArgs); err != nil {
		t.Fatal(err)
	}
	if len(gotArgs) != len(wantArgs) || gotArgs["table"] != wantArgs["table"] {
		t.Fatalf("action.args = %v, want %v (exact match against the signed statement)", gotArgs, wantArgs)
	}

	// The directive is single-use: a second consume must now report opaque 404.
	status2, body2 := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
	if status2 != http.StatusNotFound || string(body2) != opaque404Body {
		t.Fatalf("second consume = %d %s, want 404 %s", status2, body2, opaque404Body)
	}
}

// TestConsumeRevalidatesStoredStatementRejectsInvalid proves consume
// re-runs opsig.ParseStatement over the stored statement bytes rather than
// trusting a plain json.Unmarshal: a record whose bytes decode cleanly as
// JSON but fail the full validator (here, a wrong instance) must never leak
// its action — even though the store-level CAS (TargetCN/TargetGen/time
// bounds) is otherwise satisfied. Submits the record directly (bypassing
// submitDirective's use of the real instance) so the stored bytes disagree
// with b.instanceID.
func TestConsumeRevalidatesStoredStatementRejectsInvalid(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	b.Directives().BumpGeneration("manager")

	id := "11111111-2222-4333-8444-555555555640"
	now := time.Now()
	bad := directiveStatement(id, "manager", 1, instructionAction("x"))
	bad.Instance = "wrong-instance" // decodes fine; ParseStatement must reject it
	raw, err := json.Marshal(bad)
	if err != nil {
		t.Fatal(err)
	}
	dr := DirectiveRecord{
		ID: id, Statement: raw, TargetCN: "manager", TargetGen: 1,
		Kind: "instruction", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := b.Directives().Submit(dr, now); err != nil {
		t.Fatal(err)
	}

	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
	if status != http.StatusNotFound || string(body) != opaque404Body {
		t.Fatalf("consume of invalid stored statement = %d %s, want opaque 404 %s", status, body, opaque404Body)
	}
}

func TestConsumeHappyPathInstruction(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	b.Directives().BumpGeneration("manager") // generation 0 -> 1

	id := "11111111-2222-4333-8444-555555555602"
	action := instructionAction("check the backlog")
	st := directiveStatement(id, "manager", 1, action)
	submitDirective(t, b, st)

	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
	if status != http.StatusOK {
		t.Fatalf("consume status = %d, body = %s", status, body)
	}
	var resp struct {
		ID           string `json:"id"`
		Kind         string `json:"kind"`
		AdvisoryText string `json:"advisory_text"`
		Note         string `json:"note"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	if resp.ID != id || resp.Kind != "instruction" {
		t.Fatalf("response = %+v, want id=%s kind=instruction", resp, id)
	}
	if resp.AdvisoryText != "check the backlog" {
		t.Fatalf("advisory_text = %q, want %q", resp.AdvisoryText, "check the backlog")
	}
	wantNote := "advisory only — never overrides refusal of a sensitive or outbound action"
	if resp.Note != wantNote {
		t.Fatalf("note = %q, want %q (byte-exact, em-dash included)", resp.Note, wantNote)
	}
}

// TestConsumeOpaque404ForWrongAgentUnknownIDConsumedExpiredStaleGen proves
// every distinct failure mode (wrong target CN, unknown id, replay of an
// already-consumed directive, expiry, and a target whose enrolment
// generation advanced past the directive's signed generation) produces the
// exact same byte-for-byte 404 body — no oracle for which failure occurred.
func TestConsumeOpaque404ForWrongAgentUnknownIDConsumedExpiredStaleGen(t *testing.T) {
	type outcome struct {
		name   string
		status int
		body   []byte
	}
	var outcomes []outcome

	// wrong_agent: directive targets "manager"; consume attempted with a
	// "worker"-CN certificate.
	{
		b, _, _, _ := directiveTestBroker(t)
		b.Directives().BumpGeneration("manager")
		id := "11111111-2222-4333-8444-555555555610"
		submitDirective(t, b, directiveStatement(id, "manager", 1, instructionAction("x")))
		srv := jailServer(t, b)
		defer srv.Close()
		client := agentClient(t, b, signedCert(t, b, "worker"))
		status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
		outcomes = append(outcomes, outcome{"wrong_agent", status, body})
	}

	// unknown_id: no directive with this id exists at all.
	{
		b, _, _, _ := directiveTestBroker(t)
		srv := jailServer(t, b)
		defer srv.Close()
		client := agentClient(t, b, signedCert(t, b, "manager"))
		status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", "no-such-directive-id")
		outcomes = append(outcomes, outcome{"unknown_id", status, body})
	}

	// already_consumed: first consume (over HTTP, by the real target) succeeds;
	// the second attempt against the same id must be opaque 404.
	{
		b, _, _, _ := directiveTestBroker(t)
		b.Directives().BumpGeneration("manager")
		id := "11111111-2222-4333-8444-555555555611"
		submitDirective(t, b, directiveStatement(id, "manager", 1, instructionAction("x")))
		srv := jailServer(t, b)
		defer srv.Close()
		client := agentClient(t, b, signedCert(t, b, "manager"))
		if status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id); status != http.StatusOK {
			t.Fatalf("seed first consume = %d %s, want 200", status, body)
		}
		status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
		outcomes = append(outcomes, outcome{"already_consumed", status, body})
	}

	// expired: NotBefore/ExpiresAt both already in the past at submit time.
	{
		b, _, _, _ := directiveTestBroker(t)
		b.Directives().BumpGeneration("manager")
		id := "11111111-2222-4333-8444-555555555612"
		submitDirective(t, b, expiredStatement(id, "manager", 1, instructionAction("x")))
		srv := jailServer(t, b)
		defer srv.Close()
		client := agentClient(t, b, signedCert(t, b, "manager"))
		status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
		outcomes = append(outcomes, outcome{"expired", status, body})
	}

	// stale_generation: BumpGeneration runs again AFTER submit, advancing
	// "manager" past the directive's signed target generation (and marking
	// the record invalidated).
	{
		b, _, _, _ := directiveTestBroker(t)
		b.Directives().BumpGeneration("manager") // 0 -> 1
		id := "11111111-2222-4333-8444-555555555613"
		submitDirective(t, b, directiveStatement(id, "manager", 1, instructionAction("x")))
		b.Directives().BumpGeneration("manager") // 1 -> 2; invalidates the above
		srv := jailServer(t, b)
		defer srv.Close()
		client := agentClient(t, b, signedCert(t, b, "manager"))
		status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
		outcomes = append(outcomes, outcome{"stale_generation", status, body})
	}

	for _, o := range outcomes {
		if o.status != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", o.name, o.status)
		}
		if string(o.body) != opaque404Body {
			t.Errorf("%s: body = %s, want %s", o.name, o.body, opaque404Body)
		}
	}
	for i := 1; i < len(outcomes); i++ {
		if !bytes.Equal(outcomes[0].body, outcomes[i].body) {
			t.Errorf("body not byte-identical: %s=%q vs %s=%q",
				outcomes[0].name, outcomes[0].body, outcomes[i].name, outcomes[i].body)
		}
	}
}

func TestConsumeDirectivesDisabledOpaque404(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	// Leave DirectiveVerifier as nil (disabled)
	b.directiveVerifier = nil

	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	// POST consume with any id; should get opaque 404 since directives are disabled.
	status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", "any-id-works")
	if status != http.StatusNotFound {
		t.Fatalf("consume status = %d, want 404", status)
	}
	if string(body) != opaque404Body {
		t.Fatalf("consume body = %s, want %s", body, opaque404Body)
	}
}

func TestConsumeRevokedCallerDenied(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	b.Directives().BumpGeneration("manager")
	id := "11111111-2222-4333-8444-555555555620"
	submitDirective(t, b, directiveStatement(id, "manager", 1, instructionAction("x")))
	b.Revoke("manager")

	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	status, _ := postDirectiveID(t, client, srv.URL, "/directive/consume", id)
	if status != http.StatusForbidden {
		t.Fatalf("consume status for revoked caller = %d, want 403", status)
	}
}

func TestCheckTargetGatedOpaque(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	b.Directives().BumpGeneration("manager")
	id := "11111111-2222-4333-8444-555555555630"
	submitDirective(t, b, directiveStatement(id, "manager", 1, instructionAction("x")))

	srv := jailServer(t, b)
	defer srv.Close()
	managerClient := agentClient(t, b, signedCert(t, b, "manager"))
	workerClient := agentClient(t, b, signedCert(t, b, "worker"))

	// Happy path: the target itself sees the real state.
	status, body := postDirectiveID(t, managerClient, srv.URL, "/directive/check", id)
	if status != http.StatusOK {
		t.Fatalf("check status = %d, body = %s", status, body)
	}
	var resp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if resp.ID != id || resp.State != "active" {
		t.Fatalf("check response = %+v, want id=%s state=active", resp, id)
	}

	// Wrong target: a different CN gets the same opaque 404 as an unknown id.
	wrongStatus, wrongBody := postDirectiveID(t, workerClient, srv.URL, "/directive/check", id)
	unknownStatus, unknownBody := postDirectiveID(t, managerClient, srv.URL, "/directive/check", "no-such-id")

	if wrongStatus != http.StatusNotFound || string(wrongBody) != opaque404Body {
		t.Fatalf("wrong-target check = %d %s, want 404 %s", wrongStatus, wrongBody, opaque404Body)
	}
	if unknownStatus != http.StatusNotFound || string(unknownBody) != opaque404Body {
		t.Fatalf("unknown-id check = %d %s, want 404 %s", unknownStatus, unknownBody, opaque404Body)
	}
	if !bytes.Equal(wrongBody, unknownBody) {
		t.Fatalf("check 404 bodies not byte-identical: %q vs %q", wrongBody, unknownBody)
	}
}

func TestConsumeRateLimited(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	b.Directives().BumpGeneration("manager")

	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, signedCert(t, b, "manager"))

	// wantDirectiveRateLimit calls/minute; the id doesn't need to exist —
	// the rate gate runs before the store lookup.
	for i := 0; i < wantDirectiveRateLimit; i++ {
		status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", "nonexistent")
		if status == http.StatusTooManyRequests {
			t.Fatalf("call %d prematurely rate limited: %d %s", i+1, status, body)
		}
	}
	status, body := postDirectiveID(t, client, srv.URL, "/directive/consume", "nonexistent")
	if status != http.StatusTooManyRequests {
		t.Fatalf("31st call status = %d, body = %s, want 429", status, body)
	}
}

func TestConsumeCertlessDenied(t *testing.T) {
	b, _, _, _ := directiveTestBroker(t)
	srv := jailServer(t, b)
	defer srv.Close()
	client := agentClient(t, b, tls.Certificate{})

	status, _ := postDirectiveID(t, client, srv.URL, "/directive/consume", "whatever")
	if status != http.StatusForbidden {
		t.Fatalf("certless consume status = %d, want 403", status)
	}
}
