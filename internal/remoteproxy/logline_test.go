package remoteproxy

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/logging"
)

var logLineRE = regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z level=WARN msg=.* component=remote-proxy( |$)`)

// captureLog makes lever's remote-proxy logger the default for the test,
// as `lever remote serve` does, and returns what it wrote.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev, w, flags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		// Restoring slog's own default leaves the log package writing to
		// the replaced handler.
		log.SetOutput(w)
		log.SetFlags(flags)
	})
	var b bytes.Buffer
	slog.SetDefault(logging.New(&b, "remote-proxy"))
	return &b
}

func TestUpstreamFailureWithoutAuditIsAWarnLine(t *testing.T) {
	buf := captureLog(t)
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxStateKey{}, &ctxState{line: &AuditLine{}}))
	upstreamFailed(nil)(httptest.NewRecorder(), r, errors.New("dial tcp 127.0.0.1:1: connection refused"))
	line := strings.TrimSpace(buf.String())
	if !logLineRE.MatchString(line) || !strings.Contains(line, `msg="upstream request failed" component=remote-proxy err="dial tcp 127.0.0.1:1: connection refused"`) {
		t.Fatalf("line: %q", line)
	}
}

func TestAuditWriteFailureIsAWarnLine(t *testing.T) {
	buf := captureLog(t)
	write, closer, err := OpenAudit(filepath.Join(t.TempDir(), "remote-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_ = closer.Close()
	write(AuditLine{})
	line := strings.TrimSpace(buf.String())
	if !logLineRE.MatchString(line) || !strings.Contains(line, `msg="remote audit line not written"`) || !strings.Contains(line, os.ErrClosed.Error()) {
		t.Fatalf("line: %q", line)
	}
}
