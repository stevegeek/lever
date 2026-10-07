package ghpush

import (
	"context"
	"encoding/base64"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func newGit(t *testing.T) (Git, *[][]string, *[][]string) {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	var argvs, envs [][]string
	return Git{Bin: bin, Home: t.TempDir(), Timeout: time.Minute,
		Trace: func(a, e []string) { argvs = append(argvs, a); envs = append(envs, e) }}, &argvs, &envs
}

func TestGitRunHardening(t *testing.T) {
	g, argvs, envs := newGit(t)
	t.Setenv("GIT_DIR", "/should/not/leak")
	if _, err := g.Run(context.Background(), t.TempDir(), RunOpts{}, "version"); err != nil {
		t.Fatal(err)
	}
	a, e := (*argvs)[0], (*envs)[0]
	for _, want := range []string{"core.hooksPath=/dev/null", "protocol.allow=never", "protocol.https.allow=always", "transfer.fsckObjects=true", "gc.auto=0", "maintenance.auto=false", "http.followRedirects=false", "fetch.recurseSubmodules=false"} {
		if !slices.Contains(a, want) {
			t.Errorf("argv missing -c %s: %v", want, a)
		}
	}
	if slices.Contains(a, "protocol.file.allow=always") {
		t.Errorf("file protocol must be off by default")
	}
	for _, kv := range e {
		if strings.HasPrefix(kv, "GIT_DIR=") {
			t.Errorf("inherited GIT_* leaked: %s", kv)
		}
	}
	for _, want := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "HOME=" + g.Home} {
		if !slices.Contains(e, want) {
			t.Errorf("env missing %s", want)
		}
	}
}

func TestGitRunAuthOnlyInEnv(t *testing.T) {
	g, argvs, envs := newGit(t)
	_, _ = g.Run(context.Background(), t.TempDir(), RunOpts{Auth: &Auth{BaseURL: "https://github.com", Token: "ghs_SECRET"}}, "version")
	for _, a := range (*argvs)[0] {
		if strings.Contains(a, "ghs_SECRET") || strings.Contains(a, "Authorization") {
			t.Fatalf("token in argv: %q", a)
		}
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_SECRET"))
	want := "GIT_CONFIG_VALUE_0=Authorization: Basic " + b64
	if !slices.Contains((*envs)[0], want) || !slices.Contains((*envs)[0], "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader") {
		t.Fatalf("auth env missing: %v", (*envs)[0])
	}
}

func TestScrub(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_SECRET"))
	s := Scrub("fatal: ghs_SECRET and Basic "+b64+" failed", "ghs_SECRET")
	if strings.Contains(s, "ghs_SECRET") || strings.Contains(s, b64) {
		t.Fatalf("not scrubbed: %q", s)
	}
}

func TestGitRunTimeout(t *testing.T) {
	g, _, _ := newGit(t)
	g.Timeout = time.Nanosecond
	if _, err := g.Run(context.Background(), t.TempDir(), RunOpts{}, "version"); err == nil {
		t.Fatal("a call past its timeout must fail")
	}
}

func TestGitRunErrorIsScrubbed(t *testing.T) {
	g, _, _ := newGit(t)
	// ls-remote against a URL that embeds the token string in its path makes
	// git echo it in the error.
	_, err := g.Run(context.Background(), t.TempDir(), RunOpts{Auth: &Auth{BaseURL: "https://127.0.0.1:1", Token: "ghs_SECRET"}},
		"ls-remote", "https://127.0.0.1:1/ghs_SECRET/repo")
	if err == nil || strings.Contains(err.Error(), "ghs_SECRET") {
		t.Fatalf("err = %v", err)
	}
}
