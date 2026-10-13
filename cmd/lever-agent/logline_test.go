package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var agentLogLineRE = regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z level=(INFO|WARN|ERROR) msg=.* component=lever-agent cmd=[a-z-]+( |$)`)

// keepDefaultLogger restores the process's default logger after a test that
// runs a log verb (realMain installs lever's logger as the default).
func keepDefaultLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// The unattended verbs write lever's log line shape, their failure included.
func TestLogVerbsWriteLogLines(t *testing.T) {
	keepDefaultLogger(t)
	missing := filepath.Join(t.TempDir(), "no-bootstrap.json")
	for _, verb := range []string{"gateway", "renew", "serve-capability"} {
		var stderr bytes.Buffer
		code := realMain([]string{"lever-agent", verb, "-id-dir", t.TempDir(), "-bootstrap", missing}, &stderr)
		line := strings.TrimSpace(stderr.String())
		if code != 1 || !agentLogLineRE.MatchString(line) || !strings.Contains(line, "level=ERROR") ||
			!strings.Contains(line, "cmd="+verb) {
			t.Errorf("%s: exit %d, stderr %q", verb, code, line)
		}
	}
}

func TestBootWarningsAreLogLines(t *testing.T) {
	keepDefaultLogger(t)
	var stderr bytes.Buffer
	realMain([]string{"lever-agent", "boot", "-enrol-only", "-bootstrap", filepath.Join(t.TempDir(), "none.json")}, &stderr)
	stderr.Reset()
	warnf("claude settings not delivered: %v", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz")
	line := strings.TrimSpace(stderr.String())
	if !agentLogLineRE.MatchString(line) || !strings.Contains(line, `level=WARN msg="claude settings not delivered: Authorization: Bearer ***" component=lever-agent cmd=boot`) {
		t.Fatalf("boot warning: %q", line)
	}
}

// The one-shot verbs answer whoever ran them in prose, as before.
func TestCLIVerbsKeepTheirProseErrors(t *testing.T) {
	keepDefaultLogger(t)
	var stderr bytes.Buffer
	code := realMain([]string{"lever-agent", "request", "-op", "read"}, &stderr)
	if code != 1 || stderr.String() != "lever-agent: request: missing required argument \"-tool\"\n" {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := realMain([]string{"lever-agent"}, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "lever-agent: usage: lever-agent <") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}
