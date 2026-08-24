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

func (r RealRunner) run(ctx context.Context, dir string, stdin io.Reader, env map[string]string, name string, args ...string) (Result, error) {
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
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

type FakeRunner struct {
	Calls   []Call
	scripts map[string]Result
}

func NewFakeRunner() *FakeRunner { return &FakeRunner{scripts: map[string]Result{}} }

// Script registers a canned Result for a "name arg0 arg1 ..." prefix key.
func (f *FakeRunner) Script(key string, res Result) { f.scripts[key] = res }

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
			f.Calls = append(f.Calls, Call{Name: name, Args: args, Env: env, Stdin: string(in)})
			return Result{Code: 1}, fmt.Errorf("fakerunner: read stdin: %w", err)
		}
	}
	f.Calls = append(f.Calls, Call{Name: name, Args: args, Env: env, Stdin: string(in)})
	return f.scriptedResult(name, args)
}
