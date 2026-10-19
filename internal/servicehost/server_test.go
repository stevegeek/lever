package servicehost

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type keyPair struct {
	cert tls.Certificate
	fp   Fingerprint
}

func newKeyPair(t *testing.T, eku x509.ExtKeyUsage) keyPair {
	t.Helper()
	c, k, err := GenerateSelfSigned("test", eku)
	if err != nil {
		t.Fatal(err)
	}
	cert, fp, err := LoadKeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return keyPair{cert, fp}
}

type rig struct {
	t       *testing.T
	ts      *httptest.Server
	server  keyPair
	client  keyPair
	http    *http.Client
	log     *syncBuffer
	fakeLog string
}

func writeSecret(t *testing.T, dir, name, v string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeTool is the fake stdio server with a secret in its environment.
func fakeTool(t *testing.T, fakeLog string, allow ...string) Tool {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Tool{
		Name:     "fake",
		Command:  []string{exe},
		Env:      map[string]string{"FAKE_MCP": "1", "FAKE_LOG": fakeLog},
		EnvFiles: map[string]string{"SECRET_TOKEN": writeSecret(t, t.TempDir(), "token", "s3cret")},
		Allow:    allow,
	}
}

func newRig(t *testing.T, timeout time.Duration, tools ...Tool) *rig {
	t.Helper()
	return newRigFor(t, newKeyPair(t, x509.ExtKeyUsageClientAuth), timeout, tools...)
}

func newRigFor(t *testing.T, client keyPair, timeout time.Duration, tools ...Tool) *rig {
	t.Helper()
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	r := &rig{t: t, server: newKeyPair(t, x509.ExtKeyUsageServerAuth), client: client, log: &syncBuffer{}}
	cfg := &Config{Listen: "127.0.0.1:7443", ClientFingerprint: string(r.client.fp), CallTimeout: timeout, Tools: tools, dir: t.TempDir()}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg, slog.New(slog.NewTextHandler(r.log, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	r.ts = httptest.NewUnstartedServer(srv)
	r.ts.TLS = ServerTLSConfig(r.server.cert, r.client.fp)
	r.ts.Config.ErrorLog = slog.NewLogLogger(srv.log.Handler(), slog.LevelWarn)
	r.ts.StartTLS()
	t.Cleanup(r.ts.Close)
	r.http = &http.Client{Transport: Transport(r.client.cert, r.server.fp)}
	return r
}

func (r *rig) post(path, body string) (*http.Response, string) {
	r.t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.ts.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CallerHeader, "worker-a")
	resp, err := r.http.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func call(id, name, args string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + name + `","arguments":{` + args + `}}}`
}

func resultText(t *testing.T, body string) string {
	t.Helper()
	var m struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil || len(m.Result.Content) == 0 {
		t.Fatalf("not a text result: %s", body)
	}
	return m.Result.Content[0].Text
}

func TestBridgeInitializeListAndCall(t *testing.T) {
	fakeLog := filepath.Join(t.TempDir(), "fake.log")
	r := newRig(t, 0, fakeTool(t, fakeLog, "echo", "env"))

	resp, body := r.post("/mcp/fake", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"sampling":{}},"clientInfo":{"name":"c","version":"1"}}}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"name":"fake"`) || !strings.Contains(body, `"id":1`) {
		t.Fatalf("initialize: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Mcp-Session-Id") != "" {
		t.Fatal("the bridge keeps no sessions, but issued a session id")
	}
	if resp, _ := r.post("/mcp/fake", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: %d", resp.StatusCode)
	}

	_, body = r.post("/mcp/fake", `{"jsonrpc":"2.0","id":"list-1","method":"tools/list"}`)
	var list struct {
		ID     string `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err, body)
	}
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	if list.ID != "list-1" || strings.Join(names, ",") != "echo,env" {
		t.Fatalf("tools/list = id %q tools %v, want list-1 and only the ceiling's echo,env", list.ID, names)
	}

	// A large numeric id comes back exactly as sent, and the server's own
	// request and notification before the answer are handled, not passed on.
	_, body = r.post("/mcp/fake", call("12345678901234567", "echo", `"msg":"hi","n":12345678901234567`))
	if !strings.HasPrefix(body, `{"id":12345678901234567,`) && !strings.Contains(body, `"id":12345678901234567`) {
		t.Fatalf("id not preserved: %s", body)
	}
	if txt := resultText(t, body); !strings.HasSuffix(txt, "msg=hi") {
		t.Fatalf("echo = %q", txt)
	}
	logged, _ := os.ReadFile(fakeLog)
	if !strings.Contains(string(logged), `12345678901234567`) {
		t.Fatalf("argument number changed on the way: %s", logged)
	}
	if !strings.Contains(string(logged), `"id":9999,"error"`) {
		t.Fatalf("the server's roots/list request was not answered with an error:\n%s", logged)
	}
	if strings.Contains(string(logged), "sampling") {
		t.Fatalf("the agent's client capabilities reached the server:\n%s", logged)
	}
	if !strings.Contains(r.log.String(), "decision=allow tool=fake op=echo caller=worker-a") {
		t.Fatalf("no allow audit line:\n%s", r.log)
	}
}

func TestBridgeCeilingRefusesBeforeTheServer(t *testing.T) {
	fakeLog := filepath.Join(t.TempDir(), "fake.log")
	r := newRig(t, 0, fakeTool(t, fakeLog, "echo"))
	r.post("/mcp/fake", call("1", "echo", `"msg":"warm"`))
	resp, body := r.post("/mcp/fake", call("2", "forbidden", ""))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "ceiling") {
		t.Fatalf("outside the ceiling: %d %s, want 403", resp.StatusCode, body)
	}
	logged, _ := os.ReadFile(fakeLog)
	if strings.Contains(string(logged), `"forbidden"`) {
		t.Fatalf("SECURITY: a call outside the ceiling reached the server:\n%s", logged)
	}
	if !strings.Contains(r.log.String(), "decision=deny tool=fake op=forbidden caller=worker-a") {
		t.Fatalf("no deny audit line:\n%s", r.log)
	}
	// A key equal to another under case folding would be read by a Go
	// server's struct decoding in place of the one checked.
	resp, _ = r.post("/mcp/fake", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{}},"paramſ":{"name":"forbidden"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("folded key: %d, want 400", resp.StatusCode)
	}
	// An unknown field never reaches the server.
	_, body = r.post("/mcp/fake", `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"x"},"extra":1,"_meta":{"progressToken":"p"}},"other":{"name":"forbidden"}}`)
	logged, _ = os.ReadFile(fakeLog)
	if strings.Contains(string(logged), `"other"`) || strings.Contains(string(logged), `"extra"`) || strings.Contains(string(logged), `"_meta"`) {
		t.Fatalf("SECURITY: unchecked fields reached the server:\n%s", logged)
	}
	// A duplicate key is refused: parsers disagree on which one wins.
	resp, _ = r.post("/mcp/fake", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","name":"forbidden","arguments":{}}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate name key: %d, want 400", resp.StatusCode)
	}
}

func TestBridgeEnvironmentHoldsOnlyItsOwnCredentials(t *testing.T) {
	t.Setenv("OTHER_TOOL_TOKEN", "leak")
	r := newRig(t, 0, fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "env"))
	if _, body := r.post("/mcp/fake", call("1", "env", `"name":"SECRET_TOKEN"`)); resultText(t, body) != "true:s3cret" {
		t.Fatalf("env_files value: %s", body)
	}
	if _, body := r.post("/mcp/fake", call("2", "env", `"name":"OTHER_TOOL_TOKEN"`)); resultText(t, body) != "false:" {
		t.Fatalf("SECURITY: the service host's environment reached the tool: %s", body)
	}
	if strings.Contains(r.log.String(), "s3cret") {
		t.Fatalf("SECURITY: a credential reached the log:\n%s", r.log)
	}
}

func TestBridgeRestartsAfterTheServerExits(t *testing.T) {
	r := newRig(t, 0, fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "echo", "crash"))
	_, body := r.post("/mcp/fake", call("1", "echo", `"msg":"a"`))
	first := resultText(t, body)
	resp, body := r.post("/mcp/fake", call("2", "crash", ""))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("crash: %d %s, want 503", resp.StatusCode, body)
	}
	var second string
	// A server that dies right after it started is held back for
	// restartBackoff before it is started again.
	for i := 0; i < 200; i++ {
		resp, body = r.post("/mcp/fake", call("3", "echo", `"msg":"b"`))
		if resp.StatusCode == 200 {
			second = resultText(t, body)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if second == "" || strings.Fields(first)[0] == strings.Fields(second)[0] {
		t.Fatalf("after the exit: first %q, second %q (%d %s), want a new process", first, second, resp.StatusCode, body)
	}
}

func TestBridgeCallTimeout(t *testing.T) {
	r := newRig(t, 300*time.Millisecond, fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "slow"))
	r.post("/mcp/fake", `{"jsonrpc":"2.0","id":0,"method":"ping"}`)
	if resp, body := r.post("/mcp/fake", call("1", "slow", "")); resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("slow call: %d %s, want 504", resp.StatusCode, body)
	}
}

func TestServerSurface(t *testing.T) {
	r := newRig(t, 0, fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "echo"))
	for _, c := range []struct {
		method, path, body string
		want               int
		in                 string
	}{
		{"GET", "/mcp/fake", "", http.StatusMethodNotAllowed, ""},
		{"DELETE", "/mcp/fake", "", http.StatusMethodNotAllowed, ""},
		{"POST", "/mcp/other", call("1", "echo", ""), http.StatusNotFound, ""},
		{"POST", "/mcp/fake/x", call("1", "echo", ""), http.StatusNotFound, ""},
		{"POST", "/mcp/fake?x=1", call("1", "echo", ""), http.StatusNotFound, ""},
		{"POST", "/admin", "", http.StatusNotFound, ""},
		{"POST", "/mcp/fake", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, http.StatusBadRequest, ""},
		{"POST", "/mcp/fake", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, 200, "method not found"},
		{"POST", "/mcp/fake", `{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage"}`, 200, "method not found"},
		{"POST", "/mcp/fake", `{"jsonrpc":"2.0","method":"notifications/cancelled"}`, http.StatusAccepted, ""},
		{"POST", "/mcp/fake", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, http.StatusBadRequest, ""},
		{"POST", InfoPath, "", http.StatusMethodNotAllowed, ""},
	} {
		req, _ := http.NewRequest(c.method, r.ts.URL+c.path, strings.NewReader(c.body))
		resp, err := r.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want || !strings.Contains(string(b), c.in) {
			t.Errorf("%s %s %s: %d %s, want %d %q", c.method, c.path, c.body, resp.StatusCode, b, c.want, c.in)
		}
	}
	resp, err := r.http.Get(r.ts.URL + InfoPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info Info
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil || len(info.Tools) != 1 || info.Tools[0].Name != "fake" || info.Tools[0].Allow[0] != "echo" {
		t.Fatalf("info = %+v, %v", info, err)
	}
}

func TestMutualTLSRefusesOtherKeys(t *testing.T) {
	r := newRig(t, 0, fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "echo"))
	get := func(c *http.Client) error {
		resp, err := c.Get(r.ts.URL + InfoPath)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			return errors.New(resp.Status)
		}
		return err
	}
	if err := get(r.http); err != nil {
		t.Fatalf("paired client refused: %v", err)
	}
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}}}
	if get(noCert) == nil {
		t.Fatal("SECURITY: a client without a certificate was served")
	}
	other := newKeyPair(t, x509.ExtKeyUsageClientAuth)
	if get(&http.Client{Transport: Transport(other.cert, r.server.fp)}) == nil {
		t.Fatal("SECURITY: a client with another key was served")
	}
	if !strings.Contains(r.log.String(), string(other.fp)) {
		t.Fatalf("the refused client's fingerprint is not in the log:\n%s", r.log)
	}
	// The client side refuses a server whose key is not the pinned one.
	wrongPin := newKeyPair(t, x509.ExtKeyUsageServerAuth).fp
	err := get(&http.Client{Transport: Transport(r.client.cert, wrongPin)})
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("wrong server pin: %v, want ErrPinMismatch", err)
	}
	// TLS 1.2 is refused.
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{r.client.cert}}}}
	if get(old) == nil {
		t.Fatal("a TLS 1.2 client was served")
	}
}

func TestProbeClassifiesFailures(t *testing.T) {
	r := newRig(t, 0, fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "echo"))
	addr := strings.TrimPrefix(r.ts.URL, "https://")
	ctx := context.Background()
	info, err := Probe(ctx, addr, r.client.cert, r.server.fp)
	if err != nil || len(info.Tools) != 1 {
		t.Fatalf("probe: %+v %v", info, err)
	}
	step := func(err error) string {
		var pe *ProbeError
		if errors.As(err, &pe) {
			return pe.Step
		}
		return "?"
	}
	if _, err := Probe(ctx, addr, r.client.cert, newKeyPair(t, x509.ExtKeyUsageServerAuth).fp); step(err) != "pin" {
		t.Fatalf("wrong pin: %v", err)
	}
	if _, err := Probe(ctx, addr, newKeyPair(t, x509.ExtKeyUsageClientAuth).cert, r.server.fp); step(err) != "refused" {
		t.Fatalf("wrong client key: %v", err)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	downAddr := strings.TrimPrefix(down.URL, "http://")
	down.Close()
	if _, err := Probe(ctx, downAddr, r.client.cert, r.server.fp); step(err) != "unreachable" {
		t.Fatalf("down host: %v", err)
	}
}

func TestFileTransportPicksUpARotatedKey(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	fp, _, err := EnsureKeyPair(certPath, keyPath, "x", x509.ExtKeyUsageClientAuth, false)
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := LoadClientKeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	r := newRigFor(t, keyPair{cert, fp}, 0, fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "echo"))
	c := &http.Client{Transport: FileTransport(certPath, keyPath, r.server.fp)}
	get := func() int {
		resp, err := c.Get(r.ts.URL + InfoPath)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code != 200 {
		t.Fatalf("paired key from disk: %d", code)
	}
	time.Sleep(10 * time.Millisecond)
	if _, _, err := EnsureKeyPair(certPath, keyPath, "x", x509.ExtKeyUsageClientAuth, true); err != nil {
		t.Fatal(err)
	}
	if code := get(); code == 200 {
		t.Fatal("the rotated key was not picked up: the old one was still presented")
	}
}

func TestBridgeStopsWhatTheServerStarted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child.pid")
	r := newRig(t, 500*time.Millisecond, Tool{Name: "sh", Command: []string{"/bin/sh", "-c", "sleep 30 & echo $! > " + marker + "; exit 0"}, Allow: []string{"x"}})
	resp, _ := r.post("/mcp/sh", call("1", "x", ""))
	if resp.StatusCode == 200 {
		t.Fatal("a server that exits answered")
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(b))
	for i := 0; i < 100; i++ {
		if _, err := os.Stat("/proc/" + pid); err != nil {
			return
		}
		st, _ := os.ReadFile("/proc/" + pid + "/stat")
		if strings.Contains(string(st), ") Z ") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("no /proc")
	}
	t.Fatalf("the server's background child %s outlived it", pid)
}

func TestCloseDoesNotHangOnAServerThatStoppedReading(t *testing.T) {
	tool := fakeTool(t, filepath.Join(t.TempDir(), "fake.log"), "echo")
	tool.Env["FAKE_STALL"] = "1"
	r := newRig(t, time.Second, tool)
	big := strings.Repeat("x", 3<<20)
	resp, _ := r.post("/mcp/fake", call("1", "echo", `"msg":"`+big+`"`))
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("stalled server: %d, want 504", resp.StatusCode)
	}
	srv := r.ts.Config.Handler.(*Server)
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a server blocked in a write")
	}
}
