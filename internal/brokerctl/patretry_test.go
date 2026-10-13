package brokerctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

// tokenDoer answers with the status its token maps to and records the tokens
// it was called with.
type tokenDoer struct {
	token  string
	status map[string]int
	calls  *[]string
	bodies *[]string
}

func (d tokenDoer) Do(_ context.Context, _, _ string) (int, []byte, error) {
	*d.calls = append(*d.calls, d.token)
	return d.status[d.token], []byte(`{}`), nil
}

func (d tokenDoer) DoBody(_ context.Context, _, _ string, body []byte) (int, []byte, error) {
	*d.bodies = append(*d.bodies, string(body))
	return d.Do(context.Background(), "", "")
}

// TestPATRetryDoer: a 401 is retried once with the token the PAT file holds
// now, and only when it changed; any other answer is returned as is.
func TestPATRetryDoer(t *testing.T) {
	statuses := map[string]int{"old": http.StatusUnauthorized, "new": http.StatusOK, "bad": http.StatusForbidden}
	for _, tc := range []struct {
		name  string
		reads []string // successive PAT file contents
		want  int
		calls []string
	}{
		{"rotated", []string{"old", "new"}, http.StatusOK, []string{"old", "new"}},
		{"unchanged", []string{"old", "old"}, http.StatusUnauthorized, []string{"old"}},
		{"file emptied", []string{"old", ""}, http.StatusUnauthorized, []string{"old"}},
		{"no 401", []string{"bad", "new"}, http.StatusForbidden, []string{"bad"}},
		{"retry also refused", []string{"old", "old2"}, 0, []string{"old", "old2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, bodies []string
			n := 0
			load := func() string {
				tok := tc.reads[min(n, len(tc.reads)-1)]
				n++
				return tok
			}
			d := patRetryDoer{load: load, doer: func(tok string) hubapi.BodyDoer {
				return tokenDoer{token: tok, status: statuses, calls: &calls, bodies: &bodies}
			}}
			status, _, err := d.Do(context.Background(), http.MethodGet, "/api/v1/auth/me")
			if err != nil || status != tc.want {
				t.Fatalf("status %d, %v; want %d", status, err, tc.want)
			}
			if strings.Join(calls, ",") != strings.Join(tc.calls, ",") {
				t.Fatalf("tokens sent %v, want %v", calls, tc.calls)
			}
		})
	}
}

// TestPATRetryDoerResendsTheBody: a retried write sends the same body.
func TestPATRetryDoerResendsTheBody(t *testing.T) {
	var calls, bodies []string
	reads := []string{"old", "new"}
	n := 0
	d := patRetryDoer{load: func() string { tok := reads[min(n, 1)]; n++; return tok }, doer: func(tok string) hubapi.BodyDoer {
		return tokenDoer{token: tok, status: map[string]int{"old": 401, "new": 200}, calls: &calls, bodies: &bodies}
	}}
	status, _, err := d.DoBody(context.Background(), http.MethodPut, "/x", []byte(`{"a":1}`))
	if err != nil || status != 200 || strings.Join(bodies, "|") != `{"a":1}|{"a":1}` {
		t.Fatalf("status %d, %v, bodies %v", status, err, bodies)
	}
}

// TestPATRetryDoerTransportErrorIsNotRetried: a transport failure says
// nothing about the credential.
func TestPATRetryDoerTransportErrorIsNotRetried(t *testing.T) {
	var n int
	d := patRetryDoer{load: func() string { n++; return []string{"old", "new"}[min(n-1, 1)] }, doer: func(string) hubapi.BodyDoer {
		return errDoer{}
	}}
	if _, _, err := d.Do(context.Background(), http.MethodGet, "/x"); err == nil || n != 1 {
		t.Fatalf("err %v after %d PAT reads", err, n)
	}
}

type errDoer struct{}

func (errDoer) Do(context.Context, string, string) (int, []byte, error) {
	return 0, nil, errors.New("unreachable")
}
func (errDoer) DoBody(context.Context, string, string, []byte) (int, []byte, error) {
	return 0, nil, errors.New("unreachable")
}

// TestPATRetryJailCurlSendsThePinnedToken: the jail transport sends the
// token of each attempt through the runner env, and an empty PAT file never
// sends an empty bearer.
func TestPATRetryJailCurlSendsThePinnedToken(t *testing.T) {
	r := &scriptRunner{answer: func(env map[string]string, _ string) (proc.Result, error) {
		if env["SCION_HUB_TOKEN"] == "new" {
			return proc.Result{Stdout: `{"id":"u-1"}` + "\n200"}, nil
		}
		return proc.Result{Stdout: "{}\n401"}, nil
	}}
	reads := []string{"old", "new"}
	n := 0
	hc := &hubapi.Client{T: newPATRetryJailCurl(r, "http://hub", func() string { tok := reads[min(n, 1)]; n++; return tok })}
	if _, err := hc.Me(context.Background()); err != nil {
		t.Fatalf("Me after rotation: %v", err)
	}
	if got := strings.Join(r.tokens(), ","); got != "old,new" {
		t.Fatalf("tokens sent %s", got)
	}

	r2 := &scriptRunner{answer: func(map[string]string, string) (proc.Result, error) { return proc.Result{Stdout: "{}\n200"}, nil }}
	hc = &hubapi.Client{T: newPATRetryJailCurl(r2, "http://hub", func() string { return "" })}
	if _, err := hc.Me(context.Background()); err == nil || len(r2.tokens()) != 0 {
		t.Fatalf("empty PAT: %v after %d runs", err, len(r2.tokens()))
	}
}

// TestPATRetryRESTAgainstTheFile: the broker's REST client re-reads the real
// PAT file after a 401, so a token saved over the old one is used without a
// restart.
func TestPATRetryRESTAgainstTheFile(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := st.SaveControllerPAT("old-token"); err != nil {
		t.Fatal(err)
	}
	r := &scriptRunner{}
	r.answer = func(env map[string]string, _ string) (proc.Result, error) {
		if env["SCION_HUB_TOKEN"] == "old-token" {
			// The rotation lands between the read and the hub's answer.
			if err := st.SaveControllerPAT("new-token"); err != nil {
				t.Error(err)
			}
			return proc.Result{Stdout: "{}\n401"}, nil
		}
		return proc.Result{Stdout: `{"id":"u-1","email":"c@lever.local"}` + "\n200"}, nil
	}
	got, err := controllerSenderFrom(&hubapi.Client{T: newPATRetryJailCurl(r, "http://hub", controllerPAT(st))})(context.Background())
	if err != nil || got != "user:c@lever.local" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// scriptRunner is a proc.Runner whose answer sees the env and stdin of each
// run; it records each run's SCION_HUB_TOKEN.
type scriptRunner struct {
	answer func(env map[string]string, stdin string) (proc.Result, error)
	mu     sync.Mutex
	toks   []string
}

func (s *scriptRunner) tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.toks...)
}

func (s *scriptRunner) do(env map[string]string, stdin io.Reader) (proc.Result, error) {
	s.mu.Lock()
	s.toks = append(s.toks, env["SCION_HUB_TOKEN"])
	s.mu.Unlock()
	var in string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	return s.answer(env, in)
}

func (s *scriptRunner) Run(_ context.Context, env map[string]string, _ string, _ ...string) (proc.Result, error) {
	return s.do(env, nil)
}

func (s *scriptRunner) RunIn(_ context.Context, _ string, env map[string]string, _ string, _ ...string) (proc.Result, error) {
	return s.do(env, nil)
}

func (s *scriptRunner) RunStdin(_ context.Context, stdin io.Reader, env map[string]string, _ string, _ ...string) (proc.Result, error) {
	return s.do(env, stdin)
}

// scionRefuses answers like the scion CLI: scion's 401 wording for token
// "old", success for any other.
func scionRefuses(env map[string]string, _ string) (proc.Result, error) {
	if env["SCION_HUB_TOKEN"] == "old" {
		return proc.Result{Code: 1, Stderr: "Error: authentication failed, login to hub with 'scion hub auth login'"}, errors.New("exit status 1")
	}
	return proc.Result{Stdout: "ok"}, nil
}

// TestPATRetryRunner: a scion command refused with the 401 wording re-runs
// once with the rotated PAT; an unchanged PAT, another failure, or a command
// that carried no PAT is not re-run.
func TestPATRetryRunner(t *testing.T) {
	type runFn func(p patRetryRunner, env map[string]string) (proc.Result, error)
	runs := map[string]runFn{
		"Run": func(p patRetryRunner, env map[string]string) (proc.Result, error) {
			return p.Run(context.Background(), env, "scion", "list")
		},
		"RunIn": func(p patRetryRunner, env map[string]string) (proc.Result, error) {
			return p.RunIn(context.Background(), "/d", env, "scion", "list")
		},
	}
	for name, run := range runs {
		t.Run(name, func(t *testing.T) {
			r := &scriptRunner{answer: scionRefuses}
			res, err := run(patRetryRunner{r: r, load: func() string { return "new" }}, map[string]string{"SCION_HUB_TOKEN": "old", "SCION_HUB_ENABLED": "true"})
			if err != nil || res.Stdout != "ok" || strings.Join(r.tokens(), ",") != "old,new" {
				t.Fatalf("rotated: %+v, %v, tokens %v", res, err, r.tokens())
			}

			r = &scriptRunner{answer: scionRefuses}
			if _, err := run(patRetryRunner{r: r, load: func() string { return "old" }}, map[string]string{"SCION_HUB_TOKEN": "old"}); err == nil || len(r.tokens()) != 1 {
				t.Fatalf("unchanged PAT: %v after %d runs", err, len(r.tokens()))
			}

			r = &scriptRunner{answer: func(map[string]string, string) (proc.Result, error) {
				return proc.Result{Code: 1, Stderr: "Error: agent not found"}, errors.New("exit status 1")
			}}
			if _, err := run(patRetryRunner{r: r, load: func() string { return "new" }}, map[string]string{"SCION_HUB_TOKEN": "old"}); err == nil || len(r.tokens()) != 1 {
				t.Fatalf("other failure: %v after %d runs", err, len(r.tokens()))
			}

			r = &scriptRunner{answer: scionRefuses}
			if _, err := run(patRetryRunner{r: r, load: func() string { return "new" }}, nil); err != nil || len(r.tokens()) != 1 {
				t.Fatalf("no PAT in env: %v after %d runs", err, len(r.tokens()))
			}
		})
	}
}

// TestPATRetryRunnerDoesNotMutateTheCallersEnv: the retry env is a copy.
func TestPATRetryRunnerDoesNotMutateTheCallersEnv(t *testing.T) {
	env := map[string]string{"SCION_HUB_TOKEN": "old"}
	_, _ = patRetryRunner{r: &scriptRunner{answer: scionRefuses}, load: func() string { return "new" }}.Run(context.Background(), env, "scion")
	if env["SCION_HUB_TOKEN"] != "old" {
		t.Fatal("the caller's env was changed")
	}
}

// TestPATRetryRunnerStdin: a rewindable stdin is replayed whole on the
// retry; a stream that cannot be rewound is never re-run.
func TestPATRetryRunnerStdin(t *testing.T) {
	var seen []string
	r := &scriptRunner{answer: func(env map[string]string, in string) (proc.Result, error) {
		seen = append(seen, in)
		return scionRefuses(env, in)
	}}
	p := patRetryRunner{r: r, load: func() string { return "new" }}
	env := map[string]string{"SCION_HUB_TOKEN": "old"}
	if _, err := p.RunStdin(context.Background(), bytes.NewReader([]byte(`{"cfg":1}`)), env, "scion", "start"); err != nil {
		t.Fatalf("seekable stdin: %v", err)
	}
	if strings.Join(seen, "|") != `{"cfg":1}|{"cfg":1}` {
		t.Fatalf("stdin per run: %q", seen)
	}

	seen = nil
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("stream")); pw.Close() }()
	if _, err := p.RunStdin(context.Background(), pr, env, "scion", "start"); err == nil || len(seen) != 1 {
		t.Fatalf("stream stdin: %v after %d runs", err, len(seen))
	}
}

// TestPATRetryRunnerAgainstTheFile: the broker's scion client re-reads the
// real PAT file, so a PAT rotated mid-command is used on the retry.
func TestPATRetryRunnerAgainstTheFile(t *testing.T) {
	dir := t.TempDir()
	st := state.ForConfig(dir)
	if err := st.SaveControllerPAT("old"); err != nil {
		t.Fatal(err)
	}
	r := &scriptRunner{}
	r.answer = func(env map[string]string, in string) (proc.Result, error) {
		if env["SCION_HUB_TOKEN"] == "old" {
			if err := st.SaveControllerPAT("new"); err != nil {
				t.Error(err)
			}
		}
		return scionRefuses(env, in)
	}
	sc := HostScionClient(patRetryRunner{r: r, load: controllerPAT(st)}, st, "")
	if err := sc.Message(context.Background(), scion.MsgOpts{To: "agent:worker", Body: "hi", Project: filepath.Join(dir, "proj")}); err != nil {
		t.Fatalf("message after rotation: %v", err)
	}
	if got := strings.Join(r.tokens(), ","); got != "old,new" {
		t.Fatalf("tokens %s", got)
	}
	if fi, err := os.Stat(st.ControllerPAT()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("PAT file: %v %v", fi, err)
	}
}
