// Command lever-tool-github is the host-side broker tool that pushes an
// agent's branch to GitHub. The agent writes a git bundle into the tree
// (/workspace/.lever-files/github/<name>.bundle) and calls push; this tool
// checks the request, imports the bundle into its own bare mirror and pushes
// one agent/* branch with a short-lived GitHub App token. The jail never
// holds a GitHub credential. Spec: docs/superpowers/specs/2026-10-07-lever-dev-on-desktop-design.md §4.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/captool"
	"github.com/stevegeek/lever/internal/ghpush"
)

// Version is stamped by the Makefile (-X main.Version=...).
var Version = "dev"

type opts struct {
	name, backend, admin, tree, state, appID, instID, appKey, prefix string
	repos                                                            map[string]bool
	maxBundle                                                        int64
}

func parseFlags(args []string) (opts, error) {
	var o opts
	var repos string
	fs := flag.NewFlagSet("lever-tool-github", flag.ContinueOnError)
	fs.StringVar(&o.name, "name", "github", "tool/registry name")
	fs.StringVar(&o.backend, "backend", "127.0.0.1:3210", "MCP listen address (set by the broker)")
	fs.StringVar(&o.admin, "admin", "", "broker admin base URL (set by the broker)")
	fs.StringVar(&o.tree, "tree", "", "absolute instance tree path")
	fs.StringVar(&o.state, "state", "", "absolute private state dir")
	fs.StringVar(&o.appID, "app-id", "", "GitHub App id")
	fs.StringVar(&o.instID, "installation-id", "", "GitHub App installation id")
	fs.StringVar(&o.appKey, "app-key", "", "GitHub App private key (PEM, 0600)")
	fs.StringVar(&repos, "repos", "", "comma list of owner/name")
	fs.StringVar(&o.prefix, "branch-prefix", "agent/", "required branch prefix")
	fs.Int64Var(&o.maxBundle, "max-bundle", 256<<20, "bundle size cap in bytes")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	for _, r := range []struct{ flag, v string }{{"-tree", o.tree}, {"-state", o.state}, {"-app-id", o.appID}, {"-installation-id", o.instID}, {"-app-key", o.appKey}, {"-repos", repos}} {
		if r.v == "" {
			return o, fmt.Errorf("%s is required", r.flag)
		}
	}
	for _, p := range []struct{ flag, v string }{{"-tree", o.tree}, {"-state", o.state}, {"-app-key", o.appKey}} {
		if !filepath.IsAbs(p.v) {
			return o, fmt.Errorf("%s must be an absolute path, got %q", p.flag, p.v)
		}
	}
	if err := ghpush.ValidatePrefix(o.prefix); err != nil {
		return o, err
	}
	if err := refuseInTree(o.tree, "-state", o.state, "the agent could write the mirrors"); err != nil {
		return o, err
	}
	if err := refuseInTree(o.tree, "-app-key", o.appKey, "the agent could read the GitHub App key"); err != nil {
		return o, err
	}
	o.repos = map[string]bool{}
	for _, r := range strings.Split(repos, ",") {
		o.repos[strings.TrimSpace(r)] = true
	}
	if o.maxBundle <= 0 {
		return o, fmt.Errorf("-max-bundle must be positive")
	}
	return o, nil
}

// pushBackstop re-checks the hard invariants after token verification,
// whatever the token's caveats say.
func pushBackstop(repos map[string]bool, prefix string) func(captool.ValidatedContext, map[string]string) error {
	return func(c captool.ValidatedContext, a map[string]string) error {
		if c.Operation != "push" {
			return fmt.Errorf("github: backstop: only push is permitted")
		}
		if err := ghpush.ValidateRepo(a["repo"], repos); err != nil {
			return err
		}
		if err := ghpush.ValidateBranch(a["branch"], prefix); err != nil {
			return err
		}
		return ghpush.ValidateBundleName(a["bundle"])
	}
}

// asResult turns a refusal into a result the agent can read: captool maps a
// Handler error to a bare "tool error" (captool/verify.go:72), which would
// hide "branch moved; push to a new branch name" and every named refusal.
// The messages are already token-scrubbed. The tool's own audit line still
// records decision=deny.
func asResult(v any, err error) (any, error) {
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, nil
	}
	return v, nil
}

func main() {
	o, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	key, err := ghpush.LoadAppKey(o.appKey)
	if err != nil {
		log.Fatal(err)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		log.Fatal("git not found on PATH")
	}
	for _, d := range []string{o.state, filepath.Join(o.state, "tmp"), filepath.Join(o.state, "home")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			log.Fatal(err)
		}
		if err := os.Chmod(d, 0o700); err != nil {
			log.Fatal(err)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	p := &ghpush.Pusher{
		Tree: o.tree, State: o.state, Prefix: o.prefix, BaseURL: "https://github.com",
		Repos: o.repos, MaxBundle: o.maxBundle, LockWait: 30 * time.Second,
		Tokens: &ghpush.Minter{AppID: o.appID, InstallationID: o.instID, Key: key,
			APIBase: "https://api.github.com", HTTP: &http.Client{Timeout: 30 * time.Second}, Now: time.Now},
		Git: ghpush.Git{Bin: gitBin, Ceiling: o.state, Home: filepath.Join(o.state, "home"), Timeout: 5 * time.Minute},
		Log: logger,
	}
	srv, err := captool.New(captool.Config{
		Name: o.name, Version: Version, Backend: o.backend, AdminURL: o.admin, Log: logger,
		Operations: []captool.Operation{{
			Name:        "push",
			Description: "push one agent/* branch from a git bundle in /workspace/.lever-files/github/ (no force); returns the compare URL",
			Params: []captool.ParamSpec{
				{Name: "repo", Type: "string", Description: "owner/name, e.g. stevegeek/lever"},
				{Name: "branch", Type: "string", Description: "branch name, must start with " + o.prefix},
				{Name: "bundle", Type: "string", Description: "bundle file name in /workspace/.lever-files/github/"},
			},
			CaveatParam: map[string]string{"repo": "repo"},
			Backstop:    pushBackstop(o.repos, o.prefix),
			Handler: func(c captool.ValidatedContext, a map[string]string) (any, error) {
				return asResult(p.Push(context.Background(), c.Caller, a["repo"], a["branch"], a["bundle"]))
			},
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := srv.Register(context.Background()); err != nil {
		log.Fatalf("register with broker: %v", err)
	}
	log.Printf("lever-tool-github %q serving MCP on %s", o.name, o.backend)
	log.Fatal(http.ListenAndServe(o.backend, srv.Handler()))
}

// resolveExisting resolves symlinks of the nearest existing ancestor of p and
// re-appends the part that does not exist yet.
func resolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("cannot resolve %q", p)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// refuseInTree: the state dir (mirrors) and the app key must not sit in the
// tree the agent can read and write (or be the tree itself).
func refuseInTree(tree, flag, p, why string) error {
	t, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return fmt.Errorf("-tree: %w", err)
	}
	s, err := resolveExisting(p)
	if err != nil {
		return fmt.Errorf("%s: %w", flag, err)
	}
	rel, err := filepath.Rel(t, s)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s %s is inside -tree %s: %s", flag, p, tree, why)
	}
	return nil
}
