package hubapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	leverexec "github.com/stevegeek/lever/internal/exec"
)

// scriptedRunner returns a fixed Result and error for every call, and records
// the call. exec.FakeRunner cannot model a non-zero exit (its scriptedResult
// returns a nil error on a match), which is exactly the curl-failure case here.
type scriptedRunner struct {
	res  leverexec.Result
	err  error
	call leverexec.Call
}

func (s *scriptedRunner) RunIn(_ context.Context, dir string, env map[string]string, name string, args ...string) (leverexec.Result, error) {
	s.call = leverexec.Call{Name: name, Args: args, Env: env, Dir: dir}
	return s.res, s.err
}

func (s *scriptedRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (leverexec.Result, error) {
	return s.RunIn(ctx, "", env, name, args...)
}

func jailCurl(r leverexec.Runner, token string) *JailCurl {
	return &JailCurl{Runner: r, BaseURL: "http://127.0.0.1:8080", Token: func() string { return token }}
}

func TestJailCurlRunsInTheJailAndKeepsTheTokenOutOfArgv(t *testing.T) {
	r := &scriptedRunner{res: leverexec.Result{Stdout: "{\"projects\":[]}\n200"}}
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
	// argv is: -c <script> _ <METHOD> <URL>
	if len(r.call.Args) != 5 || r.call.Args[0] != "-c" {
		t.Fatalf("argv = %v, want [-c script _ METHOD URL]", r.call.Args)
	}
	if r.call.Args[3] != "GET" || r.call.Args[4] != "http://127.0.0.1:8080/api/v1/projects" {
		t.Errorf("method/url = %q %q", r.call.Args[3], r.call.Args[4])
	}
	// The PAT must travel in the environment, and the script must reference it
	// by name. A token embedded in an argument would sit in the process list.
	if r.call.Env["SCION_HUB_TOKEN"] != "pat-secret" {
		t.Errorf("token must be passed as env, got env=%v", r.call.Env)
	}
	if strings.Contains(strings.Join(r.call.Args, " "), "pat-secret") {
		t.Error("the PAT must not appear in argv")
	}
	if !strings.Contains(r.call.Args[1], "$SCION_HUB_TOKEN") {
		t.Error("the script must expand $SCION_HUB_TOKEN itself")
	}
}

func TestJailCurlReportsStatusWithoutBody(t *testing.T) {
	r := &scriptedRunner{res: leverexec.Result{Stdout: "\n204"}}
	status, body, err := jailCurl(r, "t").Do(context.Background(), "DELETE", "/x")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != 204 || len(body) != 0 {
		t.Fatalf("status=%d body=%q, want 204 and an empty body", status, body)
	}
}

func TestJailCurlRequiresAToken(t *testing.T) {
	r := &scriptedRunner{res: leverexec.Result{Stdout: "\n200"}}
	if _, _, err := jailCurl(r, "").Do(context.Background(), "GET", "/x"); err == nil {
		t.Fatal("an unminted PAT must fail rather than send an empty bearer header")
	}
}

func TestJailCurlNamesMissingCurl(t *testing.T) {
	// lever's guest provisioning installs curl, so exit 127 means the jail is
	// not provisioned — a different problem from an unreachable hub.
	r := &scriptedRunner{
		res: leverexec.Result{Code: curlNotFound, Stderr: "sh: curl: not found"},
		err: errors.New("exit status 127"),
	}
	_, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
	if err == nil || !strings.Contains(err.Error(), "curl is missing") {
		t.Fatalf("want a provisioning-specific error, got %v", err)
	}
}

func TestJailCurlSurfacesConnectionFailure(t *testing.T) {
	r := &scriptedRunner{
		res: leverexec.Result{Code: 7, Stderr: "curl: (7) Failed to connect"},
		err: errors.New("exit status 7"),
	}
	status, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x")
	if err == nil {
		t.Fatal("a connection failure must be an error")
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
		r := &scriptedRunner{res: leverexec.Result{Stdout: out}}
		if _, _, err := jailCurl(r, "t").Do(context.Background(), "GET", "/x"); err == nil {
			t.Errorf("output %q must fail, not report a status", out)
		}
	}
}

func TestJailCurlTrimsTrailingSlashOnBaseURL(t *testing.T) {
	r := &scriptedRunner{res: leverexec.Result{Stdout: "\n200"}}
	j := &JailCurl{Runner: r, BaseURL: "http://127.0.0.1:8080/", Token: func() string { return "t" }}
	if _, _, err := j.Do(context.Background(), "GET", "/api/v1/projects"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := r.call.Args[4]; got != "http://127.0.0.1:8080/api/v1/projects" {
		t.Errorf("url = %q", got)
	}
}
