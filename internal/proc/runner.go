// Package proc is the single seam to external commands (orb, docker, scion,
// iptables). Real execution uses os/exec; tests inject FakeRunner so backend
// logic is verifiable offline. Mirrors the Ruby ScionClient runner pattern.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type Result struct {
	Stdout string
	Stderr string
	Code   int
}

type Runner interface {
	// Run executes name+args with optional extra env (KEY=VALUE merged over the
	// process env). A non-zero exit returns a non-nil error AND the Result.
	Run(ctx context.Context, env map[string]string, name string, args ...string) (Result, error)
	// RunIn is like Run but executes in the given working directory. An empty dir
	// uses the process cwd.
	RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (Result, error)
	// RunStdin is like Run but feeds stdin to the command. It is the argv-only
	// way to stream host bytes into a command (a file into `<prefix> bash -c
	// 'cat > …'`, an archive into `podman load`) — no host shell, no
	// string-built pipeline, no quoting of the prefix. A nil stdin behaves
	// like Run.
	RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (Result, error)
}

type RealRunner struct{}

// ErrOutputLimit reports a command killed because its stdout or its stderr
// passed the limit WithOutputLimit set on its context.
var ErrOutputLimit = errors.New("command output passed its limit; the command was killed")

type outputLimitKey struct{}

// commandWaitDelay is how long a command's pipes may stay open after it
// exited or was killed (exec.Cmd.WaitDelay).
const commandWaitDelay = 2 * time.Second

// WithOutputLimit returns ctx carrying a cap of n bytes on each of a
// command's stdout and stderr. RealRunner kills a command that writes more
// and returns ErrOutputLimit with what it read up to the cap. It is for a
// command whose output another party controls — an exec into an agent
// container, where the agent can replace the program with one that streams
// forever — so that output cannot grow the host process without bound. The
// context travels through wrapping runners (the jail runner) unchanged.
func WithOutputLimit(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, outputLimitKey{}, n)
}

// OutputLimit reports the cap WithOutputLimit set on ctx.
func OutputLimit(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(outputLimitKey{}).(int)
	return n, ok && n > 0
}

// cappedBuffer keeps the first max bytes written to it and calls over the
// first time a write would pass them. Writes past the cap are discarded but
// reported as written, so the copying goroutine never blocks the command on
// a full pipe while it is being killed.
type cappedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	max  int
	over func()
	hit  bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room < len(p) {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		if !c.hit {
			c.hit = true
			c.over()
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (r RealRunner) run(ctx context.Context, dir string, stdin io.Reader, env map[string]string, name string, args ...string) (Result, error) {
	if n, ok := OutputLimit(ctx); ok {
		return r.runCapped(ctx, n, dir, stdin, env, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = cmd.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if dir != "" {
		cmd.Dir = dir
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	// A command killed at its context's deadline can leave a child holding
	// the output pipes (the guest side of an `orb` or `limactl` exec); without
	// WaitDelay, Run would wait for that child however long it lives.
	cmd.WaitDelay = commandWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// The command itself succeeded; only a background child it left
		// kept the pipes open past the delay. That is how it behaved before
		// the delay existed (minus the wait), so it stays a success.
		err = nil
	}
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
	}
	return res, err
}

// runCapped is run with each output stream capped at n bytes: past the cap
// the command is killed and the error is ErrOutputLimit.
func (r RealRunner) runCapped(ctx context.Context, n int, dir string, stdin io.Reader, env map[string]string, name string, args ...string) (Result, error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	if len(env) > 0 {
		cmd.Env = cmd.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if dir != "" {
		cmd.Dir = dir
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	// A killed command's own children (the guest side of an `orb` or
	// `limactl` exec) can hold the output pipes open; WaitDelay closes them
	// so Run returns soon after the kill or the context's deadline.
	cmd.WaitDelay = commandWaitDelay
	stdout := &cappedBuffer{max: n, over: cancel}
	stderr := &cappedBuffer{max: n, over: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
	}
	if stdout.hit || stderr.hit {
		if res.Code == 0 {
			res.Code = -1
		}
		return res, fmt.Errorf("%s: %w", name, ErrOutputLimit)
	}
	return res, err
}

func (r RealRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (Result, error) {
	return r.run(ctx, dir, nil, env, name, args...)
}

func (r RealRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (Result, error) {
	return r.run(ctx, "", nil, env, name, args...)
}

func (r RealRunner) RunStdin(ctx context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (Result, error) {
	return r.run(ctx, "", stdin, env, name, args...)
}

// --- test double ---

type Call struct {
	Name  string
	Args  []string
	Env   map[string]string
	Dir   string
	Stdin string // everything RunStdin read from its reader ("" for Run/RunIn)
}

// Argv renders the call as "name arg0 arg1 ..." — the same key shape Script
// matches on, so a test can compare what ran against what it scripted.
func (c Call) Argv() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// Subcommand reports whether the call is `name sub ...` (first argument sub).
func (c Call) Subcommand(name, sub string) bool {
	return c.Name == name && len(c.Args) > 0 && c.Args[0] == sub
}

// HasPrefix reports whether the call is name followed by exactly args as its
// leading arguments; further arguments may follow.
func (c Call) HasPrefix(name string, args ...string) bool {
	if c.Name != name || len(c.Args) < len(args) {
		return false
	}
	for i, a := range args {
		if c.Args[i] != a {
			return false
		}
	}
	return true
}

// ErrUnscripted is returned (wrapped) by FakeRunner for a command no Script
// key matches.
var ErrUnscripted = errors.New("unscripted command")

// FakeRunner is safe for concurrent runs (mu guards Calls and scripts while
// they run); a test reads Calls once the code under test has returned.
type FakeRunner struct {
	Calls   []Call
	scripts map[string]Result
	mu      sync.Mutex
}

func NewFakeRunner() *FakeRunner { return &FakeRunner{scripts: map[string]Result{}} }

// Script registers a canned Result for a "name arg0 arg1 ..." prefix key.
func (f *FakeRunner) Script(key string, res Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[key] = res
}

func (f *FakeRunner) scriptedResult(name string, args []string) (Result, error) {
	full := strings.TrimSpace(name + " " + strings.Join(args, " "))
	for key, res := range f.scripts {
		if full == key || strings.HasPrefix(full, key) {
			return res, nil
		}
	}
	return Result{Code: 1}, fmt.Errorf("fakerunner: %w %q", ErrUnscripted, full)
}

// CallIndex returns the index of the first recorded call pred accepts, or -1.
func (f *FakeRunner) CallIndex(pred func(Call) bool) int {
	for i, c := range f.Calls {
		if pred(c) {
			return i
		}
	}
	return -1
}

// Called reports whether any recorded call satisfies pred.
func (f *FakeRunner) Called(pred func(Call) bool) bool { return f.CallIndex(pred) >= 0 }

// Subcommand is a Called/CallIndex predicate for `name sub ...`.
func Subcommand(name, sub string) func(Call) bool {
	return func(c Call) bool { return c.Subcommand(name, sub) }
}

// ArgvPrefix is a Called/CallIndex predicate for name with exactly args as the
// leading arguments.
func ArgvPrefix(name string, args ...string) func(Call) bool {
	return func(c Call) bool { return c.HasPrefix(name, args...) }
}

// ArgvContains is a Called/CallIndex predicate: every substring appears in the
// call's rendered Argv.
func ArgvContains(subs ...string) func(Call) bool {
	return func(c Call) bool {
		argv := c.Argv()
		for _, s := range subs {
			if !strings.Contains(argv, s) {
				return false
			}
		}
		return true
	}
}

func (f *FakeRunner) RunIn(_ context.Context, dir string, env map[string]string, name string, args ...string) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Name: name, Args: args, Env: env, Dir: dir})
	return f.scriptedResult(name, args)
}

func (f *FakeRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (Result, error) {
	return f.RunIn(ctx, "", env, name, args...)
}

// RunStdin records the call with the FULL stdin content drained into
// Call.Stdin, so a test can assert on what the command would have received.
func (f *FakeRunner) RunStdin(_ context.Context, stdin io.Reader, env map[string]string, name string, args ...string) (Result, error) {
	var in []byte
	if stdin != nil {
		var err error
		if in, err = io.ReadAll(stdin); err != nil {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.Calls = append(f.Calls, Call{Name: name, Args: args, Env: env, Stdin: string(in)})
			return Result{Code: 1}, fmt.Errorf("fakerunner: read stdin: %w", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Name: name, Args: args, Env: env, Stdin: string(in)})
	return f.scriptedResult(name, args)
}
