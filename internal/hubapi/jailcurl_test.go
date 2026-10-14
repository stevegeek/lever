package hubapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// scriptedRunner exists because proc.FakeRunner cannot model a non-zero exit.
type scriptedRunner struct {
	res  proc.Result
	err  error
	call proc.Call
	// limit is the output limit the call's context carried (0 for none).
	limit int
}

func (s *scriptedRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	s.call = proc.Call{Name: name, Args: args, Env: env, Dir: dir}
	s.limit, _ = proc.OutputLimit(ctx)
	return s.res, s.err
}

func (s *scriptedRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	return s.RunIn(ctx, "", env, name, args...)
}

func (s *scriptedRunner) RunStdin(ctx context.Context, in io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := s.RunIn(ctx, "", env, name, args...)
	if in != nil {
		b, _ := io.ReadAll(in)
		s.call.Stdin = string(b)
	}
	return res, err
}

func jailCurl(r proc.Runner, token string) *JailCurl {
	return &JailCurl{Runner: r, BaseURL: "http://127.0.0.1:8080", Token: func() string { return token }}
}

func TestJailCurlRunsInTheJailAndPassesTheTokenAsEnv(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "{\"projects\":[]}\n200"}}
	status, body, err := jailCurl(r, "pat-secret").Do(context.Background(), "GET", "/api/v1/projects")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != 200 || string(body) != `{"projects":[]}` {
		t.Fatalf("status=%d body=%q", status, body)
	}

	if r.call.Name != "sh" {
		t.Fatalf("command = %q, want sh (the request must run inside the jail)", r.call.Name)
	}
	// argv is: -c <script> _ <METHOD> <URL> <CAP>
	if len(r.call.Args) != 6 || r.call.Args[0] != "-c" {
		t.Fatalf("argv = %v, want [-c script _ METHOD URL CAP]", r.call.Args)
	}
	if r.call.Args[3] != "GET" || r.call.Args[4] != "http://127.0.0.1:8080/api/v1/projects" {
		t.Errorf("method/url = %q %q", r.call.Args[3], r.call.Args[4])
	}
	// The PAT travels in the environment and the script references it by
	// name: it must never be baked into the script text, which the host argv
	// carries.
	if r.call.Env["SCION_HUB_TOKEN"] != "pat-secret" {
		t.Errorf("token must be passed as env, got env=%v", r.call.Env)
	}
	if strings.Contains(strings.Join(r.call.Args, " "), "pat-secret") {
		t.Error("the token value must not be baked into the script lever constructs")
	}
	if !strings.Contains(r.call.Args[1], "$SCION_HUB_TOKEN") {
		t.Error("the script must expand $SCION_HUB_TOKEN itself")
	}
}

func TestJailCurlReportsStatusWithoutBody(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "\n204"}}
	status, body, err := jailCurl(r, "t").Do(context.Background(), "DELETE", "/x")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != 204 || len(body) != 0 {
		t.Fatalf("status=%d body=%q, want 204 and an empty body", status, body)
	}
}

func TestJailCurlRequiresAToken(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "\n200"}}
	if _, _, err := jailCurl(r, "").Do(context.Background(), "GET", "/x"); err == nil {
		t.Fatal("an unminted PAT must fail rather than send an empty bearer header")
	}
}

func TestJailCurlNamesMissingCurl(t *testing.T) {
	// Exit 127 means the jail is not provisioned, not that the hub is
	// unreachable.
	r := &scriptedRunner{
		res: proc.Result{Code: curlNotFound, Stderr: "sh: curl: not found"},
		err: errors.New("exit status 127"),
	}
	_, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
	if !errors.Is(err, errCurlMissing) {
		t.Fatalf("want errCurlMissing, got %v", err)
	}
}

func TestJailCurlSurfacesConnectionFailure(t *testing.T) {
	exitErr := errors.New("exit status 7")
	r := &scriptedRunner{
		res: proc.Result{Code: 7, Stderr: "curl: (7) Failed to connect"},
		err: exitErr,
	}
	status, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
	if !errors.Is(err, exitErr) {
		t.Fatalf("a connection failure must wrap the runner's error, got %v", err)
	}
	if status != 0 {
		t.Errorf("status = %d, want 0 so it can never be read as an HTTP status", status)
	}
	if !strings.Contains(err.Error(), "Failed to connect") {
		t.Errorf("error should carry curl's stderr, got %v", err)
	}
}

func TestJailCurlRejectsOutputWithoutAStatusLine(t *testing.T) {
	// A truncated or hijacked response must not read as a successful request.
	for _, out := range []string{"", "just a body with no newline", "body\nnot-a-code"} {
		r := &scriptedRunner{res: proc.Result{Stdout: out}}
		if _, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x"); err == nil {
			t.Errorf("output %q must fail, not report a status", out)
		}
	}
}

func TestJailCurlRejectsAnOversizedBody(t *testing.T) {
	// Truncating would surface as a JSON decode error that blames the wrong
	// cause.
	r := &scriptedRunner{res: proc.Result{Stdout: strings.Repeat("x", DefaultReplyCap+1) + "\n200"}}
	_, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
	assertTooLarge(t, err, "GET /x", "1 MiB")
}

// assertTooLarge checks err is the reply-cap APIError naming the call and the
// cap.
func assertTooLarge(t *testing.T, err error, call, limit string) {
	t.Helper()
	var ae *APIError
	if !errors.As(err, &ae) || !ae.TooLarge {
		t.Fatalf("want a TooLarge APIError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), call) || !strings.Contains(err.Error(), limit) {
		t.Errorf("error must name the call %q and the cap %q: %v", call, limit, err)
	}
}

func TestJailCurlDefaultCapBoundary(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: strings.Repeat("x", DefaultReplyCap) + "\n200"}}
	_, body, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
	if err != nil || len(body) != DefaultReplyCap {
		t.Fatalf("a body of exactly the cap must pass: len=%d err=%v", len(body), err)
	}
	if r.call.Args[5] != "1048576" {
		t.Errorf("curl's --max-filesize argument = %q, want the default cap", r.call.Args[5])
	}
	if r.limit != DefaultReplyCap+statusTrailerRoom {
		t.Errorf("output limit = %d, want the cap plus room for the status line", r.limit)
	}
}

func TestJailCurlPerCallCap(t *testing.T) {
	ctx := WithReplyCap(context.Background(), 8<<20)
	r := &scriptedRunner{res: proc.Result{Stdout: strings.Repeat("x", 2<<20) + "\n200"}}
	if _, _, err := jailCurl(r, "t").Do(ctx, "GET", "/big"); err != nil {
		t.Fatalf("a 2 MiB body under an 8 MiB call cap must pass: %v", err)
	}
	if r.call.Args[5] != "8388608" || r.limit != 8<<20+statusTrailerRoom {
		t.Errorf("cap arg=%q output limit=%d, want the call's cap", r.call.Args[5], r.limit)
	}
	r = &scriptedRunner{res: proc.Result{Stdout: strings.Repeat("x", 8<<20+1) + "\n200"}}
	_, _, err := jailCurl(r, "t").Do(ctx, "GET", "/big")
	assertTooLarge(t, err, "GET /big", "8 MiB")
	// The cap belongs to the call, not the transport.
	r = &scriptedRunner{res: proc.Result{Stdout: strings.Repeat("x", 2<<20) + "\n200"}}
	_, _, err = jailCurl(r, "t").Do(context.Background(), "GET", "/x")
	assertTooLarge(t, err, "GET /x", "1 MiB")
}

// A killed or stopped transfer is never a reply, even when what it wrote
// before the kill ends like one.
func TestJailCurlTreatsACutTransferAsTooLarge(t *testing.T) {
	for name, r := range map[string]*scriptedRunner{
		"output limit": {res: proc.Result{Stdout: "{}\n200", Code: -1}, err: fmt.Errorf("limactl: %w", proc.ErrOutputLimit)},
		"max-filesize": {res: proc.Result{Stdout: "{}\n200", Code: curlFileSizeExceeded}, err: errors.New("exit status 63")},
	} {
		status, body, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
		if status != 0 || body != nil {
			t.Errorf("%s: status=%d body=%q, want neither", name, status, body)
		}
		assertTooLarge(t, err, "GET /x", "1 MiB")
	}
}

func TestJailCurlClassifiesUnparseableOutputAsAnswered(t *testing.T) {
	// curl exited 0, so something answered: a plain error would make doctor
	// pass while a wrong service holds the hub port.
	for _, out := range []string{"body\nnot-a-code", strings.Repeat("x", DefaultReplyCap+1) + "\n200"} {
		r := &scriptedRunner{res: proc.Result{Stdout: out}}
		_, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Errorf("output %q: want an APIError so callers can tell it answered, got %T: %v", snippet([]byte(out)), err, err)
		}
	}
}

func TestJailCurlClassifiesTransportFailureAsUnreachable(t *testing.T) {
	// A refused connection, a missing curl and an absent PAT are not answers,
	// so they must stay plain errors.
	for _, r := range []*scriptedRunner{
		{res: proc.Result{Code: 7, Stderr: "curl: (7) Failed to connect"}, err: errors.New("exit status 7")},
		{res: proc.Result{Code: curlNotFound, Stderr: "sh: curl: not found"}, err: errors.New("exit status 127")},
	} {
		_, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			t.Errorf("a transport failure must not be an APIError, got %v", err)
		}
	}
	noPAT := &scriptedRunner{res: proc.Result{Stdout: "\n200"}}
	_, _, err := jailCurl(noPAT, "").Do(context.Background(), "GET", "/x")
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("an absent PAT must not be an APIError, got %v", err)
	}
}

func TestJailCurlTrimsTrailingSlashOnBaseURL(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "\n200"}}
	j := &JailCurl{Runner: r, BaseURL: "http://127.0.0.1:8080/", Token: func() string { return "t" }}
	if _, _, err := j.Do(context.Background(), "GET", "/api/v1/projects"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := r.call.Args[4]; got != "http://127.0.0.1:8080/api/v1/projects" {
		t.Errorf("url = %q", got)
	}
}

// A write carries its body through the environment, so the JSON is never
// quoted into the script text.
func TestJailCurlDoBodySendsJSONFromEnv(t *testing.T) {
	r := &scriptedRunner{res: proc.Result{Stdout: "{}\n200"}}
	status, _, err := jailCurl(r, "pat").DoBody(context.Background(), "PUT", "/api/v1/x", []byte(`{"a":1}`))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if r.call.Env["LEVER_HUB_BODY"] != `{"a":1}` {
		t.Errorf("body must travel as env, got %v", r.call.Env)
	}
	script := r.call.Args[1]
	if !strings.Contains(script, "$LEVER_HUB_BODY") || !strings.Contains(script, "Content-Type: application/json") {
		t.Errorf("script must send the body as JSON: %q", script)
	}
	if r.call.Args[3] != "PUT" || !strings.HasSuffix(r.call.Args[4], "/api/v1/x") {
		t.Errorf("method/url = %q %q", r.call.Args[3], r.call.Args[4])
	}
}
