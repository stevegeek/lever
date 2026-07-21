package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/opsig"
)

// directiveTestDir creates a short-path scratch dir under /tmp (not
// t.TempDir(), whose macOS path can already eat most of sockaddr_un's
// ~104-byte sun_path budget) to hold both the instance config and the
// .lever-state/directive.sock the CLI dials.
func directiveTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "levdir-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// genDirectiveKey creates a fresh ed25519 SSH keypair in dir and returns
// (privPath, allowedSignersPath) with principal "operator@<instance>".
// Duplicated from opsig_test.go's genKey — test helpers are not exported
// across packages.
func genDirectiveKey(t *testing.T, dir, instance string) (string, string) {
	t.Helper()
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
	line := "operator@" + instance + " " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(as, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return priv, as
}

// writeDirectiveConfig writes a minimal canonical lever.yaml into dir. extra
// is appended raw (e.g. "operator:\n  signing_key: /path\n").
func writeDirectiveConfig(t *testing.T, dir, name, extra string) {
	t.Helper()
	body := "name: " + name + "\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\n" + extra
	if err := os.WriteFile(filepath.Join(dir, "lever.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type capturedReq struct {
	method, path, body string
}

// reqRecorder is a race-safe append/read log of requests hitting the fake
// directive UDS server.
type reqRecorder struct {
	mu   sync.Mutex
	reqs []capturedReq
}

func (r *reqRecorder) add(c capturedReq) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, c)
}

func (r *reqRecorder) all() []capturedReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]capturedReq(nil), r.reqs...)
}

func (r *reqRecorder) has(method, path string) bool {
	for _, c := range r.all() {
		if c.method == method && c.path == path {
			return true
		}
	}
	return false
}

type canned struct {
	status int // 0 => 200
	body   string
}

// startDirectiveUDS binds a real UNIX socket at <dir>/.lever-state/directive.sock
// (the exact path stateFor/DirectiveSock computes for a config living in dir)
// and serves canned responses per route, recording every request.
func startDirectiveUDS(t *testing.T, dir string, routes map[string]canned) *reqRecorder {
	t.Helper()
	stateDir := filepath.Join(dir, ".lever-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(stateDir, "directive.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sock, err)
	}
	rec := &reqRecorder{}
	mux := http.NewServeMux()
	for path, resp := range routes {
		path, resp := path, resp
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			rec.add(capturedReq{method: r.Method, path: r.URL.Path, body: string(b)})
			status := resp.status
			if status == 0 {
				status = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(resp.body))
		})
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return rec
}

func runDirective(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	root := NewRootWithBackend(defaultFactory)
	root.SetArgs(argv)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	err := root.Execute()
	return out.String(), err
}

func TestDirectiveCommandsWired(t *testing.T) {
	root := NewHostRoot()
	var subs map[string]bool
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "directive" {
			subs = map[string]bool{}
			for _, s := range c.Commands() {
				subs[s.Name()] = true
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("`lever directive` not wired into the host root")
	}
	for _, want := range []string{"send", "list", "revoke", "selftest"} {
		if !subs[want] {
			t.Errorf("directive subcommands = %v, missing %q", subs, want)
		}
	}
}

func TestDirectiveSendSignsAndPosts(t *testing.T) {
	dir := directiveTestDir(t)
	priv, as := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	rec := startDirectiveUDS(t, dir, map[string]canned{
		"/directive/resolve": {body: `{"cn":"worker1","slug":"worker1","generation":3}`},
		"/directive/send":    {body: `{"id":"whatever","delivered":true}`},
	})

	out, err := runDirective(t, "directive", "send", "worker1", "--instruction", "hello there")
	if err != nil {
		t.Fatalf("directive send: %v\noutput: %s", err, out)
	}
	if !rec.has(http.MethodGet, "/directive/resolve") {
		t.Fatalf("resolve was not called; requests: %+v", rec.all())
	}
	var sendReq capturedReq
	for _, r := range rec.all() {
		if r.method == http.MethodPost && r.path == "/directive/send" {
			sendReq = r
		}
	}
	if sendReq.body == "" {
		t.Fatalf("send was not called; requests: %+v", rec.all())
	}

	var payload struct{ Statement, Signature string }
	if err := json.Unmarshal([]byte(sendReq.body), &payload); err != nil {
		t.Fatalf("send body not JSON: %v: %s", err, sendReq.body)
	}
	raw, err := base64.StdEncoding.DecodeString(payload.Statement)
	if err != nil {
		t.Fatalf("statement not base64: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(payload.Signature)
	if err != nil {
		t.Fatalf("signature not base64: %v", err)
	}
	v := opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(opsig.NamespaceDirective, raw, sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	var st opsig.Statement
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("statement not JSON: %v", err)
	}
	if st.V != 1 || st.Instance != "testinst" {
		t.Errorf("statement v/instance = %d/%s", st.V, st.Instance)
	}
	if st.TargetAgent.CN != "worker1" || st.TargetAgent.Generation != 3 {
		t.Errorf("statement target = %+v, want cn=worker1 gen=3", st.TargetAgent)
	}
	if st.Action.Kind != "instruction" || st.Action.Text != "hello there" {
		t.Errorf("statement action = %+v", st.Action)
	}
	if !strings.Contains(out, "signing exactly these bytes") {
		t.Errorf("stdout missing operator-review banner: %q", out)
	}
}

func TestDirectiveSendActionFlag(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	rec := startDirectiveUDS(t, dir, map[string]canned{
		"/directive/resolve": {body: `{"cn":"worker1","slug":"worker1","generation":1}`},
		"/directive/send":    {body: `{"id":"x","delivered":false}`},
	})

	action := `{"kind":"tool_call","tool":"qmd","op":"search","args":{"q":"x"},"arg_binding":"exact","uses":1}`
	out, err := runDirective(t, "directive", "send", "worker1", "--action", action)
	if err != nil {
		t.Fatalf("directive send --action: %v\noutput: %s", err, out)
	}
	var sendReq capturedReq
	for _, r := range rec.all() {
		if r.method == http.MethodPost && r.path == "/directive/send" {
			sendReq = r
		}
	}
	var payload struct{ Statement, Signature string }
	_ = json.Unmarshal([]byte(sendReq.body), &payload)
	raw, _ := base64.StdEncoding.DecodeString(payload.Statement)
	var st opsig.Statement
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("statement not JSON: %v", err)
	}
	if st.Action.Kind != "tool_call" || st.Action.Tool != "qmd" || st.Action.Op != "search" {
		t.Errorf("statement action = %+v", st.Action)
	}
}

func TestDirectiveSendNotBeforeFlag(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	rec := startDirectiveUDS(t, dir, map[string]canned{
		"/directive/resolve": {body: `{"cn":"worker1","slug":"worker1","generation":1}`},
		"/directive/send":    {body: `{"id":"x","delivered":true}`},
	})

	nb := time.Now().Add(30 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
	out, err := runDirective(t, "directive", "send", "worker1", "--instruction", "hi", "--not-before", nb)
	if err != nil {
		t.Fatalf("directive send --not-before: %v\noutput: %s", err, out)
	}
	var sendReq capturedReq
	for _, r := range rec.all() {
		if r.method == http.MethodPost && r.path == "/directive/send" {
			sendReq = r
		}
	}
	var payload struct{ Statement, Signature string }
	_ = json.Unmarshal([]byte(sendReq.body), &payload)
	raw, _ := base64.StdEncoding.DecodeString(payload.Statement)
	var st opsig.Statement
	_ = json.Unmarshal(raw, &st)
	if st.NotBefore != nb {
		t.Errorf("not_before = %q, want %q", st.NotBefore, nb)
	}
}

func TestDirectiveListStateFilter(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	startDirectiveUDS(t, dir, map[string]canned{
		"/directive/list": {body: `{"directives":[{"id":"a","state":"active"},{"id":"b","state":"consumed"}]}`},
	})

	out, err := runDirective(t, "directive", "list", "--state", "active")
	if err != nil {
		t.Fatalf("directive list --state: %v\noutput: %s", err, out)
	}
	if !strings.Contains(out, `"id":"a"`) {
		t.Errorf("output missing active directive %q", out)
	}
	if strings.Contains(out, `"id":"b"`) {
		t.Errorf("output should have filtered out the consumed directive: %q", out)
	}
}

func TestDirectiveSendActionAndInstructionMutuallyExclusive(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	// No server started at all — this must fail before any network call.
	_, err := runDirective(t, "directive", "send", "worker1", "--instruction", "hi", "--action", `{"kind":"instruction","text":"hi"}`)
	if err == nil {
		t.Fatal("--instruction + --action together should error")
	}
}

func TestDirectiveSendRequiresOneOfInstructionOrAction(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	_, err := runDirective(t, "directive", "send", "worker1")
	if err == nil {
		t.Fatal("neither --instruction nor --action should error")
	}
}

func TestDirectiveSendInvalidActionRejectedClientSide(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	// No server started — an invalid action must be rejected before any
	// resolve/send round-trip.
	_, err := runDirective(t, "directive", "send", "worker1", "--action", `{"kind":"sudo"}`)
	if err == nil {
		t.Fatal("invalid action kind should be rejected client-side")
	}
}

func TestDirectiveSendExpiryBeyondCapErrorsClientSide(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n  directive_expiry_max: 1h\n")
	t.Chdir(dir)

	rec := startDirectiveUDS(t, dir, map[string]canned{
		"/directive/resolve": {body: `{"cn":"worker1","slug":"worker1","generation":1}`},
		"/directive/send":    {body: `{"id":"x","delivered":true}`},
	})

	_, err := runDirective(t, "directive", "send", "worker1", "--instruction", "hi", "--expires", "2h")
	if err == nil {
		t.Fatal("expiry beyond the configured cap should error")
	}
	if rec.has(http.MethodPost, "/directive/send") {
		t.Fatal("send must not reach the broker when the expiry cap is violated")
	}
}

func TestDirectiveSendMissingKeyErrors(t *testing.T) {
	dir := directiveTestDir(t)
	// No operator.signing_key in config, no --key flag.
	writeDirectiveConfig(t, dir, "testinst", "")
	t.Chdir(dir)

	// No server started — a missing key must be caught before any dial.
	_, err := runDirective(t, "directive", "send", "worker1", "--instruction", "hi")
	if err == nil {
		t.Fatal("missing signing key should error")
	}
}

func TestDirectiveListSendsSignedEnvelope(t *testing.T) {
	dir := directiveTestDir(t)
	priv, as := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	rec := startDirectiveUDS(t, dir, map[string]canned{
		"/directive/list": {body: `{"directives":[]}`},
	})

	out, err := runDirective(t, "directive", "list")
	if err != nil {
		t.Fatalf("directive list: %v\noutput: %s", err, out)
	}
	var listReq capturedReq
	for _, r := range rec.all() {
		if r.method == http.MethodPost && r.path == "/directive/list" {
			listReq = r
		}
	}
	if listReq.body == "" {
		t.Fatalf("list was not called; requests: %+v", rec.all())
	}
	var payload struct{ Envelope, Signature string }
	if err := json.Unmarshal([]byte(listReq.body), &payload); err != nil {
		t.Fatalf("list body not JSON: %v: %s", err, listReq.body)
	}
	raw, _ := base64.StdEncoding.DecodeString(payload.Envelope)
	sig, _ := base64.StdEncoding.DecodeString(payload.Signature)
	v := opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(opsig.NamespaceAdmin, raw, sig); err != nil {
		t.Fatalf("envelope signature does not verify: %v", err)
	}
	var env opsig.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	if env.Op != "list" || env.Instance != "testinst" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestDirectiveRevokeSendsSignedEnvelope(t *testing.T) {
	dir := directiveTestDir(t)
	priv, as := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	rec := startDirectiveUDS(t, dir, map[string]canned{
		"/directive/revoke": {body: `{"revoked":true}`},
	})

	out, err := runDirective(t, "directive", "revoke", "abc-123")
	if err != nil {
		t.Fatalf("directive revoke: %v\noutput: %s", err, out)
	}
	var revReq capturedReq
	for _, r := range rec.all() {
		if r.method == http.MethodPost && r.path == "/directive/revoke" {
			revReq = r
		}
	}
	if revReq.body == "" {
		t.Fatalf("revoke was not called; requests: %+v", rec.all())
	}
	var payload struct{ Envelope, Signature string }
	if err := json.Unmarshal([]byte(revReq.body), &payload); err != nil {
		t.Fatalf("revoke body not JSON: %v: %s", err, revReq.body)
	}
	raw, _ := base64.StdEncoding.DecodeString(payload.Envelope)
	sig, _ := base64.StdEncoding.DecodeString(payload.Signature)
	v := opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(opsig.NamespaceAdmin, raw, sig); err != nil {
		t.Fatalf("envelope signature does not verify: %v", err)
	}
	var env opsig.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope not JSON: %v", err)
	}
	if env.Op != "revoke" || env.Params["id"] != "abc-123" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestDirectiveSelftestOK(t *testing.T) {
	dir := directiveTestDir(t)
	priv, as := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	rec := startDirectiveUDS(t, dir, map[string]canned{
		"/directive/selftest": {body: `{"ok":true}`},
	})

	out, err := runDirective(t, "directive", "selftest")
	if err != nil {
		t.Fatalf("directive selftest: %v\noutput: %s", err, out)
	}
	var stReq capturedReq
	for _, r := range rec.all() {
		if r.method == http.MethodPost && r.path == "/directive/selftest" {
			stReq = r
		}
	}
	var payload struct{ Statement, Signature string }
	_ = json.Unmarshal([]byte(stReq.body), &payload)
	raw, _ := base64.StdEncoding.DecodeString(payload.Statement)
	sig, _ := base64.StdEncoding.DecodeString(payload.Signature)
	v := opsig.Verifier{AllowedSigners: as, Principal: "operator@testinst"}
	if err := v.Verify(opsig.NamespaceDirective, raw, sig); err != nil {
		t.Fatalf("selftest signature does not verify: %v", err)
	}
	var st opsig.Statement
	_ = json.Unmarshal(raw, &st)
	if st.TargetAgent.CN != "selftest" || st.TargetAgent.Generation != 1 {
		t.Errorf("selftest target = %+v, want cn=selftest gen=1", st.TargetAgent)
	}
}

func TestDirectiveSelftestFailureExitsNonZero(t *testing.T) {
	dir := directiveTestDir(t)
	priv, _ := genDirectiveKey(t, dir, "testinst")
	writeDirectiveConfig(t, dir, "testinst", "operator:\n  signing_key: "+priv+"\n")
	t.Chdir(dir)

	startDirectiveUDS(t, dir, map[string]canned{
		"/directive/selftest": {status: http.StatusBadRequest, body: `{"error":"signature verification failed"}`},
	})

	_, err := runDirective(t, "directive", "selftest")
	if err == nil {
		t.Fatal("selftest failure should return a non-nil error (non-zero exit)")
	}
}
