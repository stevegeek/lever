// Package exec is the single seam to external commands (orb, docker, scion,
// iptables). Real execution uses os/exec; tests inject FakeRunner so backend
// logic is verifiable offline. Mirrors the Ruby ScionClient runner pattern.
package exec

import (
	"bytes"
	"context"
	"fmt"
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
}

type RealRunner struct{}

func (r RealRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (Result, error) {
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		res.Code = ee.ExitCode()
	}
	return res, err
}

func (r RealRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// --- test double ---

type Call struct {
	Name string
	Args []string
	Env  map[string]string
	Dir  string
}

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
	return Result{Code: 1}, fmt.Errorf("fakerunner: unscripted command %q", full)
}

func (f *FakeRunner) RunIn(_ context.Context, dir string, env map[string]string, name string, args ...string) (Result, error) {
	f.Calls = append(f.Calls, Call{Name: name, Args: args, Env: env, Dir: dir})
	return f.scriptedResult(name, args)
}

func (f *FakeRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (Result, error) {
	return f.RunIn(ctx, "", env, name, args...)
}
