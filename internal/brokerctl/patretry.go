package brokerctl

import (
	"context"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
)

// A hub 401 to the controller PAT is retried once, and only when the PAT file
// now holds a different token: apply saves a rotated PAT before it revokes
// the old one, so a call that read the old token just before the revoke
// races it. A 401 means the hub refused the credential and did nothing, so
// the retry cannot repeat an effect. An unchanged token is never retried: the
// same credential would be refused again.

// freshPAT returns the PAT the file holds now when it differs from used.
func freshPAT(load func() string, used string) (string, bool) {
	tok := load()
	return tok, tok != "" && tok != used
}

// patRetryDoer is the broker's controller-PAT REST transport. Each attempt
// pins the token it sends, so the token compared after a 401 is the one the
// hub refused rather than a second read of the file.
type patRetryDoer struct {
	load func() string
	// doer returns the transport that sends with token.
	doer func(token string) hubapi.BodyDoer
}

func newPATRetryJailCurl(r proc.Runner, baseURL string, load func() string) patRetryDoer {
	return patRetryDoer{load: load, doer: func(token string) hubapi.BodyDoer {
		return &hubapi.JailCurl{Runner: r, BaseURL: baseURL, Token: func() string { return token }}
	}}
}

func (d patRetryDoer) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return d.with(func(bd hubapi.BodyDoer) (int, []byte, error) { return bd.Do(ctx, method, path) })
}

func (d patRetryDoer) DoBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	return d.with(func(bd hubapi.BodyDoer) (int, []byte, error) { return bd.DoBody(ctx, method, path, body) })
}

func (d patRetryDoer) with(call func(hubapi.BodyDoer) (int, []byte, error)) (int, []byte, error) {
	used := d.load()
	status, body, err := call(d.doer(used))
	if err != nil || status != http.StatusUnauthorized {
		return status, body, err
	}
	tok, changed := freshPAT(d.load, used)
	if !changed {
		return status, body, err
	}
	return call(d.doer(tok))
}

// hubTokenEnv is the env key the scion client sends the controller PAT in.
const hubTokenEnv = "SCION_HUB_TOKEN"

// hubAuthRefused is scion's wording when the hub answers 401 to the CLI's
// credential.
const hubAuthRefused = "authentication failed, login to hub"

// patRetryRunner is the broker scion client's runner. Only a command that
// carried a PAT and failed with scion's 401 wording is retried. One scion
// command can make several hub calls with the same token; if the token was
// revoked between them the retry re-runs the earlier ones; that needs a
// revoke landing inside one command after its first hub call succeeded.
type patRetryRunner struct {
	r    proc.Runner
	load func() string
}

func (p patRetryRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := p.r.Run(ctx, env, name, args...)
	if retryEnv, ok := p.retry(res, err, env); ok {
		return p.r.Run(ctx, retryEnv, name, args...)
	}
	return res, err
}

func (p patRetryRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (proc.Result, error) {
	res, err := p.r.RunIn(ctx, dir, env, name, args...)
	if retryEnv, ok := p.retry(res, err, env); ok {
		return p.r.RunIn(ctx, dir, retryEnv, name, args...)
	}
	return res, err
}

// RunStdin retries only when stdin can be rewound to where it started: a
// consumed stream would re-run the command with less input.
func (p patRetryRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	seeker, _ := stdin.(io.Seeker)
	var start int64
	if seeker != nil {
		off, serr := seeker.Seek(0, io.SeekCurrent)
		if serr != nil {
			seeker = nil
		}
		start = off
	}
	res, err := p.r.RunStdin(ctx, stdin, env, name, args...)
	if stdin != nil && seeker == nil {
		return res, err
	}
	retryEnv, ok := p.retry(res, err, env)
	if !ok {
		return res, err
	}
	if seeker != nil {
		if _, serr := seeker.Seek(start, io.SeekStart); serr != nil {
			return res, err
		}
	}
	return p.r.RunStdin(ctx, stdin, retryEnv, name, args...)
}

func (p patRetryRunner) retry(res proc.Result, err error, env map[string]string) (map[string]string, bool) {
	if err == nil || p.load == nil {
		return nil, false
	}
	used, ok := env[hubTokenEnv]
	if !ok || used == "" {
		return nil, false
	}
	if !strings.Contains(res.Stdout, hubAuthRefused) && !strings.Contains(res.Stderr, hubAuthRefused) {
		return nil, false
	}
	tok, changed := freshPAT(p.load, used)
	if !changed {
		return nil, false
	}
	out := maps.Clone(env)
	out[hubTokenEnv] = tok
	return out, true
}
