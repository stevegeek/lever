package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
	"github.com/stevegeek/lever/internal/scion"
)

// genOperatorKey creates a fresh ed25519 SSH keypair in a temp dir and
// returns (privPath, allowedSignersPath) with principal "operator@testinst".
// Duplicated from opsig_test.go's genKey — test helpers are not exported
// across packages.
func genOperatorKey(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	priv := filepath.Join(dir, "opkey")
	out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", priv, "-C", "op", "-q").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(priv + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(pub)) // type key comment
	as := filepath.Join(dir, "allowed_signers")
	line := "operator@testinst " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(as, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return priv, as
}

// fakeDirectiveRuntime embeds fakeRuntime (worker_test.go) and additionally
// records every Message call, so directive-delivery tests can assert on the
// recipient + body without touching the shared fake.
type fakeDirectiveRuntime struct {
	fakeRuntime
	messages []scion.MsgOpts
}

func (f *fakeDirectiveRuntime) Message(ctx context.Context, o scion.MsgOpts) error {
	f.messages = append(f.messages, o)
	return f.fakeRuntime.Message(ctx, o)
}

// directiveTestBroker builds a Broker wired for the directive admin channel:
// a real ssh-keygen operator key/allowed_signers pair, instance "testinst",
// a 24h expiry cap, a declared "worker" WorkerSpec, and a message-capturing
// fake runtime.
func directiveTestBroker(t *testing.T) (b *Broker, priv string, allowedSigners string, rt *fakeDirectiveRuntime) {
	t.Helper()
	priv, as := genOperatorKey(t)
	cfg := testConfig(t)
	cfg.DirectiveVerifier = &opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	cfg.InstanceID = "testinst"
	cfg.DirectiveExpiryMax = 24 * time.Hour
	rt = &fakeDirectiveRuntime{fakeRuntime: fakeRuntime{agents: map[string][]scion.Agent{}}}
	cfg.Runtime = rt
	cfg.Workers = []WorkerSpec{{Name: "worker", WorkspaceSubdir: "workers/worker"}}
	cfg.InstanceProject = testInstanceProject
	return New(cfg), priv, as, rt
}

// serveDirectiveAdmin binds b.DirectiveAdminHandler() on a real unix socket
// and returns the socket path. Uses os.MkdirTemp (not t.TempDir) because
// t.TempDir paths can exceed macOS's ~104-byte unix socket path limit.
func serveDirectiveAdmin(t *testing.T, b *Broker) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "d")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %q: %v", sock, err)
	}
	srv := &http.Server{Handler: b.DirectiveAdminHandler()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// directiveClient returns an http.Client dialing the given unix socket for
// every request, regardless of host/port in the URL.
func directiveClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

func directiveStatement(id, cn string, gen int, action opsig.Action) opsig.Statement {
	now := time.Now()
	return opsig.Statement{
		V: 1, Instance: "testinst", DirectiveID: id,
		TargetAgent: opsig.Target{CN: cn, Generation: gen},
		IssuedAt:    now.Format(time.RFC3339),
		NotBefore:   now.Add(-time.Minute).Format(time.RFC3339),
		ExpiresAt:   now.Add(10 * time.Minute).Format(time.RFC3339),
		Action:      action,
	}
}

func instructionAction(text string) opsig.Action {
	return opsig.Action{Kind: "instruction", Text: text}
}

// postRaw signs raw under namespace with priv and POSTs the {field1,field2}
// b64std envelope the send/list/revoke/selftest routes all share, returning
// the response status and body.
func postSigned(t *testing.T, client *http.Client, path, priv, namespace string, raw []byte) (int, []byte) {
	t.Helper()
	sig, err := opsig.Sign(priv, namespace, raw)
	if err != nil {
		t.Fatal(err)
	}
	return postSignedWithSig(t, client, path, raw, sig)
}

func postSignedWithSig(t *testing.T, client *http.Client, path string, raw, sig []byte) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		fieldNameFor(path): base64.StdEncoding.EncodeToString(raw),
		"signature":        base64.StdEncoding.EncodeToString(sig),
	})
	resp, err := client.Post("http://unix"+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, got
}

// fieldNameFor returns the JSON envelope-field name for path: /directive/send
// and /directive/selftest carry {"statement":...}; /directive/list and
// /directive/revoke carry {"envelope":...}.
func fieldNameFor(path string) string {
	if path == "/directive/list" || path == "/directive/revoke" {
		return "envelope"
	}
	return "statement"
}

func postSend(t *testing.T, client *http.Client, priv string, st opsig.Statement) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return postSigned(t, client, "/directive/send", priv, opsig.NamespaceDirective, raw)
}

func postSelftest(t *testing.T, client *http.Client, priv string, st opsig.Statement) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return postSigned(t, client, "/directive/selftest", priv, opsig.NamespaceDirective, raw)
}

func adminEnvelope(op string, params map[string]string) opsig.Envelope {
	return opsig.Envelope{V: 1, Instance: "testinst", Op: op, Params: params, IssuedAt: time.Now().Format(time.RFC3339)}
}

func postEnvelope(t *testing.T, client *http.Client, path, priv string, env opsig.Envelope) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return postSigned(t, client, path, priv, opsig.NamespaceAdmin, raw)
}

func TestDirectiveSendVerifiesStoresAndDelivers(t *testing.T) {
	b, priv, _, rt := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	b.Directives().BumpGeneration("manager") // generation 0 -> 1

	id := "11111111-2222-4333-8444-555555555501"
	st := directiveStatement(id, "manager", 1, instructionAction("do the thing"))
	code, body := postSend(t, client, priv, st)
	if code != http.StatusOK {
		t.Fatalf("send status = %d, body = %s", code, body)
	}
	var resp struct {
		ID        string `json:"id"`
		Delivered bool   `json:"delivered"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	if resp.ID != id || !resp.Delivered {
		t.Fatalf("send response = %+v, want id=%s delivered=true", resp, id)
	}

	recs := b.Directives().List(time.Now())
	if len(recs) != 1 || recs[0].ID != id || recs[0].State != "active" {
		t.Fatalf("store after send = %+v", recs)
	}

	if len(rt.messages) != 1 {
		t.Fatalf("Message calls = %d, want 1", len(rt.messages))
	}
	msg := rt.messages[0]
	if msg.To != "agent:manager" { // manager slug defaults to ManagerIdentity ("manager")
		t.Fatalf("delivered To = %q, want agent:manager", msg.To)
	}
	if !strings.Contains(msg.Body, id) {
		t.Fatalf("delivered body missing directive id: %q", msg.Body)
	}
	if strings.Contains(msg.Body, "do the thing") {
		t.Fatalf("delivered body leaked action content (must be pointer-only): %q", msg.Body)
	}
}

func TestDirectiveSendRejectsBadSignatureAndNothingDelivered(t *testing.T) {
	b, priv, _, rt := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	b.Directives().BumpGeneration("manager")

	id := "11111111-2222-4333-8444-555555555502"
	st := directiveStatement(id, "manager", 1, instructionAction("hi"))
	raw, _ := json.Marshal(st)
	sig, err := opsig.Sign(priv, opsig.NamespaceDirective, raw)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append(bytes.Clone(raw), ' ') // flip/append one byte -> signature no longer matches
	code, _ := postSignedWithSig(t, client, "/directive/send", tampered, sig)
	if code != http.StatusBadRequest {
		t.Fatalf("tampered send status = %d, want 400", code)
	}
	if recs := b.Directives().List(time.Now()); len(recs) != 0 {
		t.Fatalf("store not empty after rejected send: %+v", recs)
	}
	if len(rt.messages) != 0 {
		t.Fatalf("Message calls = %d, want 0", len(rt.messages))
	}
}

func TestDirectiveSendRejectsStaleGenerationAndUnknownTargetAndDupID(t *testing.T) {
	b, priv, _, _ := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	b.Directives().BumpGeneration("manager") // generation -> 1

	// Stale generation: statement signed for generation 2, store is at 1.
	staleID := "11111111-2222-4333-8444-555555555511"
	code, _ := postSend(t, client, priv, directiveStatement(staleID, "manager", 2, instructionAction("x")))
	if code != http.StatusConflict {
		t.Fatalf("stale generation status = %d, want 409", code)
	}

	// Unknown target: CN maps to neither the manager nor a declared worker.
	unknownID := "11111111-2222-4333-8444-555555555512"
	code, _ = postSend(t, client, priv, directiveStatement(unknownID, "ghost-agent", 1, instructionAction("x")))
	if code != http.StatusBadRequest {
		t.Fatalf("unknown target status = %d, want 400", code)
	}

	// Duplicate id: first send at (manager, gen 1) succeeds, resend of the
	// identical id is rejected.
	dupID := "11111111-2222-4333-8444-555555555513"
	code, body := postSend(t, client, priv, directiveStatement(dupID, "manager", 1, instructionAction("x")))
	if code != http.StatusOK {
		t.Fatalf("first send status = %d, body = %s", code, body)
	}
	code, _ = postSend(t, client, priv, directiveStatement(dupID, "manager", 1, instructionAction("x")))
	if code != http.StatusConflict {
		t.Fatalf("dup id status = %d, want 409", code)
	}

	recs := b.Directives().List(time.Now())
	if len(recs) != 1 {
		t.Fatalf("store should hold exactly the one successful send, got %+v", recs)
	}
}

func TestDirectiveSendRejectsToolCallForUnknownTool(t *testing.T) {
	b, priv, _, _ := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	b.Directives().BumpGeneration("manager")

	id := "11111111-2222-4333-8444-555555555521"
	action := opsig.Action{Kind: "tool_call", Tool: "no-such-tool", Op: "read", ArgBinding: "exact", Uses: 1}
	code, body := postSend(t, client, priv, directiveStatement(id, "manager", 1, action))
	if code != http.StatusBadRequest {
		t.Fatalf("unknown-tool send status = %d, want 400, body=%s", code, body)
	}
	if recs := b.Directives().List(time.Now()); len(recs) != 0 {
		t.Fatalf("store not empty after rejected tool_call: %+v", recs)
	}
}

// TestDirectiveSendRejectsExpiryBeyondInstanceCap proves the handler's own
// b.directiveExpiryMax clamp (internal/broker/directive_admin.go, checked
// AFTER opsig.ParseStatement) actually fires: a statement well inside
// opsig's own 24h hard ceiling must still be rejected once it exceeds a
// tighter, instance-configured cap.
func TestDirectiveSendRejectsExpiryBeyondInstanceCap(t *testing.T) {
	priv, as := genOperatorKey(t)
	cfg := testConfig(t)
	cfg.DirectiveVerifier = &opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	cfg.InstanceID = "testinst"
	cfg.DirectiveExpiryMax = time.Hour // tighter than opsig's 24h MaxExpiry
	rt := &fakeDirectiveRuntime{fakeRuntime: fakeRuntime{agents: map[string][]scion.Agent{}}}
	cfg.Runtime = rt
	cfg.InstanceProject = testInstanceProject
	b := New(cfg)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	b.Directives().BumpGeneration("manager")

	now := time.Now()
	st := opsig.Statement{
		V: 1, Instance: "testinst", DirectiveID: "11111111-2222-4333-8444-555555555561",
		TargetAgent: opsig.Target{CN: "manager", Generation: 1},
		IssuedAt:    now.Format(time.RFC3339),
		NotBefore:   now.Add(-time.Minute).Format(time.RFC3339),
		ExpiresAt:   now.Add(2 * time.Hour).Format(time.RFC3339), // > 1h instance cap, well < opsig's 24h cap
		Action:      instructionAction("x"),
	}
	code, body := postSend(t, client, priv, st)
	if code != http.StatusBadRequest {
		t.Fatalf("over-instance-cap send status = %d, want 400, body=%s", code, body)
	}
	if recs := b.Directives().List(time.Now()); len(recs) != 0 {
		t.Fatalf("store not empty after over-cap send: %+v", recs)
	}
	if len(rt.messages) != 0 {
		t.Fatalf("over-cap send must not deliver: %+v", rt.messages)
	}
}

func TestDirectiveListAndRevokeRequireFreshSignedEnvelope(t *testing.T) {
	b, priv, _, _ := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	b.Directives().BumpGeneration("manager")
	id := "11111111-2222-4333-8444-555555555531"
	if code, body := postSend(t, client, priv, directiveStatement(id, "manager", 1, instructionAction("x"))); code != http.StatusOK {
		t.Fatalf("seed send status = %d, body=%s", code, body)
	}

	// Unsigned: garbage signature bytes.
	rawList, _ := json.Marshal(adminEnvelope("list", nil))
	if code, _ := postSignedWithSig(t, client, "/directive/list", rawList, []byte("not-a-signature")); code != http.StatusBadRequest {
		t.Fatalf("unsigned list status = %d, want 400", code)
	}

	// Stale: validly signed but issued_at far outside the freshness window.
	staleEnv := opsig.Envelope{V: 1, Instance: "testinst", Op: "list", IssuedAt: time.Now().Add(-10 * time.Minute).Format(time.RFC3339)}
	rawStale, _ := json.Marshal(staleEnv)
	if code, _ := postSigned(t, client, "/directive/list", priv, opsig.NamespaceAdmin, rawStale); code != http.StatusBadRequest {
		t.Fatalf("stale list status = %d, want 400", code)
	}

	// Tampered: valid signature, mutated envelope bytes.
	env := adminEnvelope("list", nil)
	rawOK, _ := json.Marshal(env)
	sig, err := opsig.Sign(priv, opsig.NamespaceAdmin, rawOK)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append(bytes.Clone(rawOK), ' ')
	if code, _ := postSignedWithSig(t, client, "/directive/list", tampered, sig); code != http.StatusBadRequest {
		t.Fatalf("tampered list status = %d, want 400", code)
	}

	// Valid list: 200, contains the seeded directive.
	code, body := postEnvelope(t, client, "/directive/list", priv, adminEnvelope("list", nil))
	if code != http.StatusOK {
		t.Fatalf("valid list status = %d, body=%s", code, body)
	}
	var listResp struct {
		Directives []DirectiveRecord `json:"directives"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	if len(listResp.Directives) != 1 || listResp.Directives[0].ID != id {
		t.Fatalf("list content = %+v, want the seeded directive %s", listResp.Directives, id)
	}

	// Valid revoke.
	code, body = postEnvelope(t, client, "/directive/revoke", priv, adminEnvelope("revoke", map[string]string{"id": id}))
	if code != http.StatusOK {
		t.Fatalf("valid revoke status = %d, body=%s", code, body)
	}
	var revResp struct {
		Revoked bool `json:"revoked"`
	}
	if err := json.Unmarshal(body, &revResp); err != nil || !revResp.Revoked {
		t.Fatalf("revoke response = %s, err=%v", body, err)
	}

	// Revoking an unknown id succeeds the request but reports revoked:false.
	code, body = postEnvelope(t, client, "/directive/revoke", priv, adminEnvelope("revoke", map[string]string{"id": "no-such-id"}))
	if code != http.StatusOK {
		t.Fatalf("unknown-id revoke status = %d, want 200, body=%s", code, body)
	}
	if err := json.Unmarshal(body, &revResp); err != nil || revResp.Revoked {
		t.Fatalf("unknown-id revoke response = %s, want revoked:false", body)
	}
}

func TestSelftestVerifyOnlyNoSideEffects(t *testing.T) {
	b, priv, _, rt := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	// Target may be a dummy: no generation bump, unknown CN — selftest must
	// not check target-known/generation, only verify+parse.
	st := directiveStatement("11111111-2222-4333-8444-555555555541", "some-dummy-agent", 1, instructionAction("probe"))
	code, body := postSelftest(t, client, priv, st)
	if code != http.StatusOK {
		t.Fatalf("selftest status = %d, body=%s", code, body)
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || !resp.OK {
		t.Fatalf("selftest response = %s, err=%v", body, err)
	}
	if recs := b.Directives().List(time.Now()); len(recs) != 0 {
		t.Fatalf("selftest must not store: %+v", recs)
	}
	if len(rt.messages) != 0 {
		t.Fatalf("selftest must not deliver: %+v", rt.messages)
	}

	// Bad-signature selftest surfaces the underlying verify error (the
	// allowed_signers-misconfig probe) rather than a terse message.
	raw, _ := json.Marshal(st)
	badCode, badBody := postSignedWithSig(t, client, "/directive/selftest", raw, []byte("garbage"))
	if badCode != http.StatusBadRequest {
		t.Fatalf("bad-sig selftest status = %d, want 400", badCode)
	}
	if len(strings.TrimSpace(string(badBody))) == 0 {
		t.Fatal("bad-sig selftest must return a non-empty verify error body")
	}
}

func TestDirectiveRoutesDisabledWithoutVerifier(t *testing.T) {
	b := New(testConfig(t)) // no DirectiveVerifier set -> nil
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	checks := []struct {
		method, path string
	}{
		{http.MethodPost, "/directive/send"},
		{http.MethodGet, "/directive/resolve?agent=manager"},
		{http.MethodPost, "/directive/list"},
		{http.MethodPost, "/directive/revoke"},
		{http.MethodPost, "/directive/selftest"},
	}
	for _, c := range checks {
		req, err := http.NewRequest(c.method, "http://unix"+c.path, bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s with nil verifier: status = %d, want 404", c.method, c.path, resp.StatusCode)
		}
	}
}

func TestAuditLogRotatesPastCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "directives.log")
	a := newDirectiveAudit(path)

	blob := strings.Repeat("x", 1024)
	for i := 0; i < 1100; i++ { // ~1100 * ~1050 bytes > 1 MiB
		a.append("issued", map[string]any{"id": fmt.Sprintf("id-%d", i), "blob": blob})
	}

	rotated, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("rotated file %s.1 missing: %v", path, err)
	}
	if rotated.Size() == 0 {
		t.Fatal("rotated file is empty")
	}
	fresh, err := os.Stat(path)
	if err != nil {
		t.Fatalf("fresh log missing: %v", err)
	}
	if fresh.Size() > 1<<20 {
		t.Fatalf("fresh log not rotated, size = %d", fresh.Size())
	}
}

// TestKeyRevocationByEditingAllowedSignersIsLive proves the stolen-key
// response: truncating allowed_signers on disk takes effect on the VERY NEXT
// call, no broker restart, because Verify shells out to ssh-keygen -Y verify
// against the file path every time.
func TestKeyRevocationByEditingAllowedSignersIsLive(t *testing.T) {
	b, priv, allowedSigners, _ := directiveTestBroker(t)
	sock := serveDirectiveAdmin(t, b)
	client := directiveClient(sock)

	b.Directives().BumpGeneration("manager")

	id := "11111111-2222-4333-8444-555555555551"
	st := directiveStatement(id, "manager", 1, instructionAction("first"))
	raw, _ := json.Marshal(st)
	sig, err := opsig.Sign(priv, opsig.NamespaceDirective, raw)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := postSignedWithSig(t, client, "/directive/send", raw, sig); code != http.StatusOK {
		t.Fatalf("first send status = %d, body=%s", code, body)
	}

	// Live-revoke the operator key by truncating allowed_signers.
	if err := os.WriteFile(allowedSigners, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Re-send with a DIFFERENT id but the SAME key/signature material shape
	// (still same key) -- must now fail verification with no restart.
	id2 := "11111111-2222-4333-8444-555555555552"
	st2 := directiveStatement(id2, "manager", 1, instructionAction("second"))
	raw2, _ := json.Marshal(st2)
	sig2, err := opsig.Sign(priv, opsig.NamespaceDirective, raw2)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := postSignedWithSig(t, client, "/directive/send", raw2, sig2)
	if code != http.StatusBadRequest {
		t.Fatalf("post-revocation send status = %d, want 400 (key must be dead immediately)", code)
	}

	recs := b.Directives().List(time.Now())
	if len(recs) != 1 || recs[0].ID != id {
		t.Fatalf("store changed after revoked-key send: %+v", recs)
	}
}
