package logging

import (
	"bytes"
	"errors"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

var lineRE = regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z level=(INFO|WARN|ERROR) msg=.* component=broker( |$)`)

func TestNewLineShapeAndRedaction(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "broker")
	l.Info("start podman run -e CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abcdefghijklmnop", "detail", "token ghs_ABCDEFGHIJKLMNOPQRSTUV", "err", errors.New("Authorization: Bearer abcdefghijklmnopqrstuvwxyz"))
	l.Info("raw sk-ant-api03-abcdefghijklmnop")
	l.Warn("x", "flag", "scion server start --session-secret=hunter2hunter2")
	out := b.String()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !lineRE.MatchString(line) {
			t.Errorf("line shape: %q", line)
		}
	}
	for _, leak := range []string{"abcdefghijklmnop", "ABCDEFGHIJKLMNOPQRSTUV", "abcdefghijklmnopqrstuvwxyz", "hunter2"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	for _, kept := range []string{"CLAUDE_CODE_OAUTH_TOKEN=***", "raw sk-ant-***", "ghs_***", "Bearer ***", "--session-secret=***"} {
		if !strings.Contains(out, kept) {
			t.Errorf("missing %q in %q", kept, out)
		}
	}
}

func TestInstallRoutesTheStdlibLog(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var b bytes.Buffer
	Install(&b, "broker")
	log.Printf("http: TLS handshake error from 10.0.0.1:1234: remote error: tls: bad certificate")
	if !Installed() {
		t.Fatal("not installed")
	}
	got := strings.TrimSpace(b.String())
	if !lineRE.MatchString(got) || !strings.Contains(got, "tls: bad certificate") {
		t.Fatalf("stdlib line: %q", got)
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{"worker w1 started", "token bucket refilled", "-e FOO", "the key is set",
		"reading token /Users/stephen/.lever-state/controller.pat failed",
		"refreshing the token periodically-renewed-thing",
		"the hub token expires in 9h59m0s", "--token-file /path is unset"} {
		if got := Redact(s); got != s {
			t.Errorf("Redact(%q) = %q", s, got)
		}
	}
}

func TestWriterLogsProseLinesWithTheirLevel(t *testing.T) {
	var b bytes.Buffer
	w := Writer(New(&b, "remote-proxy"))
	w.Write([]byte("remote proxy \"x\" serving on 127.0.0.1:8445\nlever: warning: remote.files is on"))
	w.Write([]byte(" for contacts\n\njail: error: boom\n"))
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines: %q", lines)
	}
	if !strings.Contains(lines[0], `level=INFO msg="remote proxy \"x\" serving on 127.0.0.1:8445"`) ||
		!strings.Contains(lines[1], `level=WARN msg="remote.files is on for contacts" component=remote-proxy from=lever`) ||
		!strings.Contains(lines[2], `level=ERROR msg=boom component=remote-proxy from=jail`) {
		t.Fatalf("lines: %q", lines)
	}
}

func TestRedactMasksCredentialShapes(t *testing.T) {
	cases := map[string]string{
		"Authorization: Basic dXNlcjpwYXNz":                    "Authorization: Basic ***",
		"authorization=Bearer abcdefghijklmnopqrstuvwx":        "authorization=Bearer ***",
		"X-Lever-Token: 0123456789abcdef":                      "X-Lever-Token: ***",
		"token: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig-part": "token: eyJ***",
		"pat scion_pat_abcdefghijkl in use":                    "pat scion_pat_*** in use",
		"scion server start --session-secret VALUE123":         "scion server start --session-secret ***",
		"x --api-token=abc":                                    "x --api-token=***",
		"use bearer abcdefghijklmnopqrstuvwxyz now":            "use bearer *** now",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}
