package captool

import (
	"bytes"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

func TestNewLoggerWritesLeversLineShape(t *testing.T) {
	var b bytes.Buffer
	NewLogger(&b, "lever-tool-todo").Info("serving", "detail", "Authorization: Bearer abcdefghijklmnopqrstuvwxyz")
	line := strings.TrimSpace(b.String())
	want := regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z level=INFO msg=serving component=lever-tool-todo detail="Authorization: Bearer \*\*\*"$`)
	if !want.MatchString(line) {
		t.Fatalf("line: %q", line)
	}
}

// The decision line keeps its message and fields, and its detail is masked
// whatever handler the tool passed.
func TestDecisionLineIsMaskedUnderAnyHandler(t *testing.T) {
	s := testServer(t)
	var b bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&b, nil))
	s.audit("read", "worker", "error", "backend: GITHUB_TOKEN ghs_ABCDEFGHIJKLMNOPQRSTUV refused")
	out := b.String()
	if strings.Contains(out, "ABCDEFGHIJKLMNOPQRSTUV") {
		t.Fatalf("decision line leaks a token: %q", out)
	}
	if !strings.Contains(out, `msg=captool.decision tool=db op=read caller=worker decision=error detail="backend: GITHUB_TOKEN ghs_*** refused"`) {
		t.Fatalf("decision line: %q", out)
	}
}
