package ghpush

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// Git runs the git CLI with a scrubbed environment and fixed hardening
// options. Nothing from the user's or the system's git config applies, no
// hook can run, and only https (plus file on the one import call) is allowed.
type Git struct {
	Bin       string // absolute path to git
	Home      string // an empty, private directory
	Ceiling   string // GIT_CEILING_DIRECTORIES: git never looks above this
	Timeout   time.Duration
	AllowHTTP bool                     // tests only: allow plain http remotes
	Trace     func(args, env []string) // tests only
}

// Auth makes one git call authenticate to BaseURL with a GitHub token.
type Auth struct{ BaseURL, Token string }

// RunOpts are the per-call exceptions.
type RunOpts struct {
	AllowFile bool   // the bundle import reads a local file
	Auth      *Auth  // ls-remote and push only
	GitDir    string // pin the repository: git never searches parent directories
}

var hardening = []string{
	"core.hooksPath=/dev/null",
	"core.fsmonitor=false",
	"transfer.fsckObjects=true",
	"fetch.fsckObjects=true",
	"fetch.recurseSubmodules=false",
	"gc.auto=0",
	"maintenance.auto=false",
	"http.followRedirects=false",
	"credential.helper=",
	"protocol.allow=never",
	"protocol.https.allow=always",
	// index-pack streams blobs over 16 MiB instead of holding them, and
	// resolves deltas on one thread: one delta base and result in memory at
	// a time (PackLimits caps their size).
	"core.bigFileThreshold=16m",
	"pack.threads=1",
}

func basicHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// Scrub removes the raw token and its Basic-auth form from s, then applies
// lever's general secret redaction.
func Scrub(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)), "[REDACTED]")
		s = strings.ReplaceAll(s, token, "[REDACTED]")
	}
	return scion.RedactSecrets(s)
}

// Run executes git in dir and returns its stdout. Errors carry scrubbed stderr.
func (g Git) Run(ctx context.Context, dir string, o RunOpts, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()
	argv := []string{}
	for _, h := range hardening {
		argv = append(argv, "-c", h)
	}
	if g.AllowHTTP {
		argv = append(argv, "-c", "protocol.http.allow=always")
	}
	if o.AllowFile {
		argv = append(argv, "-c", "protocol.file.allow=always")
	}
	if o.GitDir != "" {
		argv = append(argv, "--git-dir="+o.GitDir)
	}
	argv = append(argv, args...)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.Home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
	if g.Ceiling != "" {
		env = append(env, "GIT_CEILING_DIRECTORIES="+g.Ceiling)
	}
	token := ""
	if o.Auth != nil {
		token = o.Auth.Token
		env = append(env, "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+o.Auth.BaseURL+"/.extraheader",
			"GIT_CONFIG_VALUE_0="+basicHeader(token))
	}
	if g.Trace != nil {
		g.Trace(append([]string{}, argv...), append([]string{}, env...))
	}
	cmd := exec.CommandContext(ctx, g.Bin, argv...)
	cmd.Dir, cmd.Env = dir, env
	// A child (git-remote-https) can hold the pipes open after the kill.
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// Scrub BEFORE truncating: a token cut at the edge would no longer
		// match the exact-match scrub.
		msg := Scrub(strings.TrimSpace(stderr.String()), token)
		if len(msg) > 4000 {
			msg = msg[:4000] + "…"
		}
		verb := ""
		if len(args) > 0 {
			verb = args[0]
		}
		return stdout.String(), fmt.Errorf("git %s: %v: %s", verb, err, msg)
	}
	return stdout.String(), nil
}

// MinGitVersion is the oldest git whose fetch from a bundle runs fsck on
// the objects (transfer.fsckObjects, honored for bundles since 2.46.0).
// Older git imports a bundle unchecked.
var MinGitVersion = [3]int{2, 46, 0}

// CheckVersion refuses a git older than MinGitVersion.
func (g Git) CheckVersion(ctx context.Context, dir string) error {
	out, err := g.Run(ctx, dir, RunOpts{}, "version")
	if err != nil {
		return err
	}
	v, ok := parseGitVersion(out)
	if !ok {
		return fmt.Errorf("cannot read the git version from %q", strings.TrimSpace(out))
	}
	for i := range v {
		if v[i] != MinGitVersion[i] {
			if v[i] < MinGitVersion[i] {
				return fmt.Errorf("%s is too old: git %d.%d.%d or newer runs fsck on a bundle import", strings.TrimSpace(out), MinGitVersion[0], MinGitVersion[1], MinGitVersion[2])
			}
			break
		}
	}
	return nil
}

// parseGitVersion reads "git version 2.49.0" (also "2.39.5 (Apple Git-154)"
// and "2.46.0.windows.1").
func parseGitVersion(s string) ([3]int, bool) {
	var v [3]int
	f := strings.Fields(s)
	if len(f) < 3 || f[0] != "git" || f[1] != "version" {
		return v, false
	}
	parts := strings.Split(f[2], ".")
	if len(parts) < 2 {
		return v, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			if i < 2 {
				return v, false
			}
			break // "2.46.rc0": treat the patch level as 0
		}
		v[i] = n
	}
	return v, true
}
