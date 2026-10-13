package broker

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/scion"
)

// The broker.decision line is parsed by operators' greps and by lever
// doctor: under lever's logger it keeps its message and the order of its
// fixed fields, and its detail is masked.
func TestAuditLineShapeIsStable(t *testing.T) {
	b := newTestBroker(t, &fakeRuntime{agents: map[string][]scion.Agent{}}, WorkerSpec{Name: "worker"})
	var buf bytes.Buffer
	b.log = logging.New(&buf, "broker")
	b.audit("request", "hello", "deny", "runtime: podman run -e CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abcdefghijklmnop", "id", "t1")
	line := strings.TrimSpace(buf.String())
	want := regexp.MustCompile(`^time=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z level=INFO msg=broker\.decision component=broker op=request caller=hello decision=deny detail="runtime: podman run -e CLAUDE_CODE_OAUTH_TOKEN=\*\*\*" id=t1$`)
	if !want.MatchString(line) {
		t.Fatalf("audit line: %q", line)
	}
}
