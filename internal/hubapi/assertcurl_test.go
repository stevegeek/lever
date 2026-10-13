package hubapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

const testAssertion = "eyJhbGciOiJFUzI1NiJ9.assertion-secret.sig"

func assertCurl(r proc.Runner) (*AssertCurl, *int) {
	signed := 0
	return &AssertCurl{Runner: r, BaseURL: "http://127.0.0.1:8080", Sign: func() (string, error) {
		signed++
		return testAssertion, nil
	}}, &signed
}

func TestAssertCurlSendsTheAssertionThroughEnvAndNeverABearer(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "{\"id\":\"u\"}\n200"}}
	a, signed := assertCurl(r)
	status, body, err := a.Do(context.Background(), "GET", "/api/v1/auth/me")
	if err != nil || status != 200 || string(body) != `{"id":"u"}` {
		t.Fatalf("status=%d body=%q err=%v", status, body, err)
	}
	if *signed != 1 {
		t.Fatalf("signed %d times, want once per request", *signed)
	}
	if r.call.Name != "sh" || len(r.call.Args) != 5 || r.call.Args[0] != "-c" {
		t.Fatalf("argv = %q %q, want sh -c script _ METHOD URL", r.call.Name, r.call.Args)
	}
	if r.call.Args[3] != "GET" || r.call.Args[4] != "http://127.0.0.1:8080/api/v1/auth/me" {
		t.Fatalf("method/url = %q %q", r.call.Args[3], r.call.Args[4])
	}
	// The jail runner moves exactly this key to stdin.
	if r.call.Env[jail.HubAssertionEnv] != testAssertion {
		t.Fatalf("assertion must travel as %s, got env %v", jail.HubAssertionEnv, r.call.Env)
	}
	if _, has := r.call.Env["SCION_HUB_TOKEN"]; has {
		t.Fatal("an assertion request must not carry the PAT")
	}
	joined := strings.Join(r.call.Args, " ")
	if strings.Contains(joined, "assertion-secret") {
		t.Fatalf("assertion baked into the argv: %q", joined)
	}
	for _, script := range []string{assertCurlScript, assertCurlBodyScript} {
		if strings.Contains(strings.ToLower(script), "authorization") || strings.Contains(script, "SCION_HUB_TOKEN") {
			t.Fatalf("script must never send a bearer: %q", script)
		}
		if !strings.Contains(script, "-H @-") || !strings.Contains(script, "printf '"+AssertionHeader+": %s\\n'") {
			t.Fatalf("script must feed the header to curl on stdin: %q", script)
		}
		if !strings.Contains(script, "curl -q ") || !strings.Contains(script, "--noproxy '*'") {
			t.Fatalf("script must ignore .curlrc and proxy variables: %q", script)
		}
	}
}

// Through the real jail runner the host argv carries neither the value nor
// the PAT key; the assertion is the first stdin line.
func TestAssertCurlThroughTheJailRunnerKeepsTheAssertionOffTheHostArgv(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("limactl", proc.Result{Stdout: "{}\n201"})
	jr := jail.New(jail.Config{Host: host, Prefix: []string{"limactl", "shell", "lever-x"}, UID: "501"})
	a, _ := assertCurl(jr)
	if _, _, err := a.DoBody(context.Background(), "POST", "/api/v1/admin/users/invite", []byte(`{"email":"a@b"}`)); err != nil {
		t.Fatalf("DoBody: %v", err)
	}
	call := host.Calls[0]
	argv := strings.Join(append([]string{call.Name}, call.Args...), " ")
	if strings.Contains(argv, "assertion-secret") {
		t.Fatalf("assertion on the host argv: %q", argv)
	}
	if call.Stdin != testAssertion+"\n" {
		t.Fatalf("stdin = %q, want the assertion line", call.Stdin)
	}
	if !strings.Contains(argv, "LEVER_HUB_BODY={\"email\":\"a@b\"}") {
		t.Fatalf("the body rides the env like JailCurl's: %q", argv)
	}
}

// A real sh and curl against a local server: the hub sees the assertion
// header with the exact value, and no Authorization header.
func TestAssertCurlScriptWithRealCurl(t *testing.T) {
	// The jail env pins PATH to these directories.
	found := false
	for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		if _, err := os.Stat(dir + "/curl"); err == nil {
			found = true
		}
	}
	if !found {
		t.Skip("curl not installed on the jail PATH")
	}
	type seen struct {
		method, path, assertion, auth, accept, ctype, body string
		hasAuth                                            bool
	}
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, hasAuth := r.Header["Authorization"]
		got = seen{r.Method, r.URL.RequestURI(), r.Header.Get(AssertionHeader), r.Header.Get("Authorization"),
			r.Header.Get("Accept"), r.Header.Get("Content-Type"), string(b), hasAuth}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	jr := jail.New(jail.Config{Host: proc.RealRunner{}, UID: "501"})
	a := &AssertCurl{Runner: jr, BaseURL: srv.URL, Sign: func() (string, error) { return testAssertion, nil }}
	status, body, err := a.DoBody(context.Background(), "POST", "/api/v1/auth/tokens", []byte(`{"name":"x"}`))
	if err != nil || status != 201 || string(body) != `{"ok":true}` {
		t.Fatalf("status=%d body=%q err=%v", status, body, err)
	}
	want := seen{"POST", "/api/v1/auth/tokens", testAssertion, "", "application/json", "application/json", `{"name":"x"}`, false}
	if got != want {
		t.Fatalf("server saw %+v\nwant %+v", got, want)
	}
	if _, _, err := a.Do(context.Background(), "GET", "/api/v1/users?search=a%40b"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got.method != "GET" || got.path != "/api/v1/users?search=a%40b" || got.assertion != testAssertion || got.hasAuth || got.body != "" {
		t.Fatalf("server saw %+v", got)
	}
}

func TestAssertionPathAllowed(t *testing.T) {
	allowed := []struct{ method, path string }{
		{"GET", "/api/v1/auth/me"},
		{"POST", "/api/v1/admin/users/invite"},
		{"GET", "/api/v1/admin/roles"},
		{"POST", "/api/v1/admin/access-constraint-previews"},
		{"DELETE", "/api/v1/admin/role-bindings/b-1"},
		{"GET", "/api/v1/admin/role-bindings?principalType=user&principalId=u-1"},
		{"GET", "/api/v1/users"},
		{"GET", "/api/v1/users?search=a%40b&limit=200"},
		{"PATCH", "/api/v1/users/u-1"},
		{"GET", "/api/v1/auth/tokens"},
		{"POST", "/api/v1/auth/tokens"},
		{"POST", "/api/v1/auth/tokens/t-1/revoke"},
		{"POST", "/api/v1/projects/register"},
		{"PUT", "/api/v1/system/identity"},
		{"GET", "/api/v1/runtime-brokers"},
		{"GET", "/api/v1/projects"},
		{"GET", "/api/v1/projects/p-1"},
		{"GET", "/api/v1/projects/p-1/providers"},
	}
	for _, c := range allowed {
		if err := AssertionPathAllowed(c.method, c.path); err != nil {
			t.Errorf("%s %s: refused: %v", c.method, c.path, err)
		}
	}
	refused := []struct{ method, path string }{
		{"GET", "/api/v1/agents/x/ports/80"},
		{"GET", "/api/v1/agents/x/ports/80/proxy/"},
		{"GET", "/api/v1/projects/p/agents/x/ports/80/proxy/"},
		{"GET", "/api/v1/projects/p/agents/x/PORTS"},
		{"GET", "/api/v1/usersx"},
		{"GET", "/api/v1/auth/tokensfoo"},
		{"GET", "/api/v1/auth/meow"},
		{"GET", "/api/v1/runtime-brokers-x"},
		{"POST", "/api/v1/projects"},
		{"PUT", "/api/v1/projects/p-1/settings"},
		{"DELETE", "/api/v1/projects/p-1/shared-dirs/scratchpad"},
		{"POST", "/api/v1/projects/p-1/providers"},
		{"GET", "/api/v1/users/../agents"},
		{"GET", "/api/v1/users/.."},
		{"GET", "/api/v1/users/./x"},
		{"GET", "/api/v1/users/%2e%2e/agents"},
		{"GET", "/api/v1/users/%2E%2E/agents"},
		{"GET", "/api/v1/users/x%2fy"},
		{"GET", "/api/v1/users/x%2Fy"},
		{"GET", "/api/v1/users/x%5cy"},
		{"GET", "/api/v1/users//x"},
		{"GET", "/api/v1/users\\x"},
		{"GET", "/api/v1/users/x\n"},
		{"GET", "/api/v1/users/x\r"},
		{"GET", "/api/v1/users/x\x00"},
		{"GET", "/api/v1/users/x\x7f"},
		{"GET", "/api/v1/users/x y"},
		{"GET", "/api/v1/users#x"},
		{"GET", "/api/v1/users?q=a\nb"},
		{"GET", "api/v1/users"},
		{"GET", "http://evil/api/v1/users"},
		{"GET", ""},
		{"GET", "/"},
		{"GET", "/api/v1/agents"},
		{"GET", "/api/v1/admin"},
		{"GET", "/web/"},
		{"get", "/api/v1/users"},
		{"OPTIONS", "/api/v1/users"},
		{"GET /x", "/api/v1/users"},
	}
	for _, c := range refused {
		err := AssertionPathAllowed(c.method, c.path)
		if !errors.Is(err, ErrAssertionPathRefused) {
			t.Errorf("%q %q: want ErrAssertionPathRefused, got %v", c.method, c.path, err)
		}
	}
}

// The allowlist is checked before signing: a refused path never mints an
// assertion and never reaches the jail.
func TestAssertCurlRefusesBeforeSigning(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/agents/x/ports/80"},
		{"GET", "/api/v1/usersx"},
		{"GET", "/api/v1/auth/tokensfoo"},
		{"POST", "/api/v1/projects"},
		{"GET", "/api/v1/users/../agents"},
		{"GET", "/api/v1/users/%2e%2e/agents"},
	} {
		r := &scriptedRunner{res: proc.Result{Stdout: "\n200"}}
		a, signed := assertCurl(r)
		_, _, err := a.Do(context.Background(), c.method, c.path)
		if !errors.Is(err, ErrAssertionPathRefused) {
			t.Errorf("%s %s: want ErrAssertionPathRefused, got %v", c.method, c.path, err)
		}
		if *signed != 0 || r.call.Name != "" {
			t.Errorf("%s %s: signed=%d ran=%q; nothing may happen for a refused path", c.method, c.path, *signed, r.call.Name)
		}
	}
}

func TestAssertCurlFailsClosedWithoutAnAssertion(t *testing.T) {
	signErr := errors.New("no key")
	for name, sign := range map[string]func() (string, error){
		"nil":   nil,
		"error": func() (string, error) { return "", signErr },
		"empty": func() (string, error) { return "", nil },
	} {
		r := &scriptedRunner{res: proc.Result{Stdout: "\n200"}}
		a := &AssertCurl{Runner: r, BaseURL: "http://h", Sign: sign}
		_, _, err := a.Do(context.Background(), "GET", "/api/v1/auth/me")
		if err == nil {
			t.Errorf("%s signer: want an error", name)
		}
		var ae *APIError
		if errors.As(err, &ae) {
			t.Errorf("%s signer: an unsigned request is not an answer, got APIError %v", name, err)
		}
		if r.call.Name != "" {
			t.Errorf("%s signer: nothing may run", name)
		}
	}
}

// curl reads --data-binary @name as a file.
func TestAssertCurlRefusesABodyCurlWouldReadAsAFile(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "\n200"}}
	a, signed := assertCurl(r)
	if _, _, err := a.DoBody(context.Background(), "POST", "/api/v1/auth/tokens", []byte("@/etc/passwd")); err == nil {
		t.Fatal("want an error")
	}
	if *signed != 0 || r.call.Name != "" {
		t.Fatal("nothing may be signed or run")
	}
}

// Transport handling is JailCurl's: a missing curl, an unreachable hub and
// unparseable output keep their classification.
func TestAssertCurlSharesJailCurlErrorHandling(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Code: curlNotFound, Stderr: "sh: curl: not found"}, err: errors.New("exit status 127")}
	a, _ := assertCurl(r)
	if _, _, err := a.Do(context.Background(), "GET", "/api/v1/auth/me"); !errors.Is(err, errCurlMissing) {
		t.Fatalf("want errCurlMissing, got %v", err)
	}
	exitErr := errors.New("exit status 7")
	r = &scriptedRunner{res: proc.Result{Code: 7, Stderr: "curl: (7) Failed to connect"}, err: exitErr}
	a, _ = assertCurl(r)
	if _, _, err := a.Do(context.Background(), "GET", "/api/v1/auth/me"); !errors.Is(err, exitErr) {
		t.Fatalf("want the runner's error, got %v", err)
	}
	r = &scriptedRunner{res: proc.Result{Stdout: "body\nnot-a-code"}}
	a, _ = assertCurl(r)
	_, _, err := a.Do(context.Background(), "GET", "/api/v1/auth/me")
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an APIError, got %v", err)
	}
	r = &scriptedRunner{res: proc.Result{Stdout: strings.Repeat("x", maxBody+1) + "\n200"}}
	a, _ = assertCurl(r)
	if _, _, err := a.Do(context.Background(), "GET", "/api/v1/auth/me"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("want a too-large error, got %v", err)
	}
}

func TestAssertCurlDoBodySendsJSONFromEnv(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "{}\n200"}}
	a, _ := assertCurl(r)
	if _, _, err := a.DoBody(context.Background(), "PATCH", "/api/v1/users/u-1", []byte(`{"role":"member"}`)); err != nil {
		t.Fatalf("DoBody: %v", err)
	}
	if r.call.Env["LEVER_HUB_BODY"] != `{"role":"member"}` || r.call.Args[1] != assertCurlBodyScript {
		t.Fatalf("env=%v script=%q", r.call.Env, r.call.Args[1])
	}
	if r.call.Args[3] != "PATCH" {
		t.Fatalf("method = %q", r.call.Args[3])
	}
}

// TestAssertCurlScriptsTurnOffGlobbing: curl expands [] and {} in a URL into
// several requests, which would send one signed assertion more than once.
func TestAssertCurlScriptsTurnOffGlobbing(t *testing.T) {
	for _, s := range []string{assertCurlScript, assertCurlBodyScript} {
		if !strings.Contains(s, "exec curl -q -g ") {
			t.Fatalf("script lacks -g:\n%s", s)
		}
	}
}
