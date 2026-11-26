package host

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/testutil"
	"github.com/stevegeek/lever/internal/wire"
)

// startOperatorUDS serves canned answers on the operator socket `lever msg
// send` dials, recording every request.
func startOperatorUDS(t *testing.T, dir string, resp canned) *reqRecorder {
	t.Helper()
	stateDir := filepath.Join(dir, ".lever-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := startUDSAt(t, filepath.Join(stateDir, "operator.sock"), map[string]canned{wire.PathOperatorNote: resp})
	return rec
}

func msgDir(t *testing.T) string {
	t.Helper()
	dir := directiveTestDir(t)
	writeInstanceInto(t, dir, instanceYAML("assistant", managerYAML+scratchWorkerYAML))
	return dir
}

// The note goes to the operator socket with the recipient, joined body and
// interrupt flag, never to scion directly.
func TestHostMsgSendPostsToTheOperatorSocket(t *testing.T) {
	for _, tc := range []struct {
		args      []string
		to        string
		interrupt bool
	}{
		{[]string{"msg", "send", "hello", "there", "--to", "assistant"}, "assistant", false},
		{[]string{"msg", "send", "hello", "there", "--to", "scratch", "--interrupt"}, "scratch", true},
	} {
		dir := msgDir(t)
		t.Chdir(dir)
		rec := startOperatorUDS(t, dir, canned{body: `{"id":"0123456789abcdef0123456789abcdef"}`})
		fr := proc.NewFakeRunner()
		out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: fr}), tc.args...)
		if err != nil {
			t.Fatalf("msg send: %v", err)
		}
		reqs := rec.all()
		if len(reqs) != 1 || reqs[0].method != http.MethodPost || reqs[0].path != wire.PathOperatorNote {
			t.Fatalf("requests = %+v", reqs)
		}
		var got wire.OperatorNoteRequest
		if err := json.Unmarshal([]byte(reqs[0].body), &got); err != nil {
			t.Fatal(err)
		}
		if got.To != tc.to || got.Body != "hello there" || got.Interrupt != tc.interrupt {
			t.Fatalf("request = %+v", got)
		}
		if len(fr.Calls) != 0 {
			t.Fatalf("msg send called the jail directly: %+v", fr.Calls)
		}
		if !strings.Contains(out, "0123456789abcdef0123456789abcdef") {
			t.Fatalf("output %q does not name the ref", out)
		}
	}
}

// TestHostMsgSendWithoutTheBrokerRefuses: with no operator socket the note is
// not sent, and the error says why and what to do.
func TestHostMsgSendWithoutTheBrokerRefuses(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	fr := proc.NewFakeRunner()
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: fr}), "msg", "send", "hi", "--to", "assistant")
	testutil.WantErrContaining(t, err, "no operator socket", "lever up", "lever attach")
	if len(fr.Calls) != 0 {
		t.Fatal("msg send must never call scion itself")
	}
}

// TestHostMsgSendReportsTheBrokersRefusal: a broker answer that is not 200
// (an unknown agent, a note it could not record) is the command's error.
func TestHostMsgSendReportsTheBrokersRefusal(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startOperatorUDS(t, dir, canned{status: http.StatusBadGateway, body: "note not sent: cannot record the message"})
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "msg", "send", "hi", "--to", "assistant")
	testutil.WantErrContaining(t, err, "cannot record")
}

func TestHostMsgSendUnknownRecipientErrors(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startOperatorUDS(t, dir, canned{body: `{}`})
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "msg", "send", "hi", "--to", "nope")
	if err == nil {
		t.Fatal("want error for unknown --to")
	}
	testutil.WantErrContaining(t, err, "nope", "assistant", "scratch")
	if len(rec.all()) != 0 {
		t.Fatalf("the broker was asked about an unknown recipient: %+v", rec.all())
	}
}

// A note to an agent that is not running is queued: the command says so,
// how many wait and why it was not woken, and passes --wake on.
func TestHostMsgSendQueued(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	rec := startOperatorUDS(t, dir, canned{body: `{"id":"","queued":true,"waiting":3,"phase":"suspended","note":"not woken: busy: too many workers are running"}`})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "msg", "send", "hi", "--to", "scratch", "--wake")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Queued for scratch, not delivered", "phase suspended", "3 message(s) wait", "not woken: busy"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
	var got wire.OperatorNoteRequest
	if err := json.Unmarshal([]byte(rec.all()[0].body), &got); err != nil || !got.Wake {
		t.Fatalf("request %+v: %v", got, err)
	}
}

// A full queue exits 9: nothing was sent or queued.
func TestHostMsgSendQueueFullExitCode(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	startOperatorUDS(t, dir, canned{status: http.StatusConflict, body: "worker scratch is not running (phase suspended); its message queue is full",
		header: map[string]string{broker.ReasonHeader: broker.ReasonQueueFull}})
	_, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "msg", "send", "hi", "--to", "scratch")
	if exitCodeOf(err) != 9 || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("exit %d: %v", exitCodeOf(err), err)
	}
}

// `lever msg list` shows the queue with the sender's text sanitised and cut.
func TestHostMsgList(t *testing.T) {
	dir := msgDir(t)
	t.Chdir(dir)
	stateDir := filepath.Join(dir, ".lever-state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"max_messages":20,"max_bytes":262144,"expiry":"168h0m0s","messages":[{"id":"ab","to":"scratch","kind":"worker:w","queued_at":"2026-01-02T03:04:05Z","expires_at":"2999-01-02T03:04:05Z","bytes":9,"body":"hi \u001b]0;x\u0007there` + strings.Repeat("y", 200) + `"}]}`
	rec := startUDSAt(t, filepath.Join(stateDir, "operator.sock"), map[string]canned{broker.PathOperatorMsgQueue: {body: body}})
	out, err := clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "msg", "list", "--to", "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "scratch") || !strings.Contains(out, "worker:w") || strings.Contains(out, "\x1b") || strings.Contains(out, strings.Repeat("y", 100)) {
		t.Fatalf("output %q", out)
	}
	if reqs := rec.all(); len(reqs) != 1 || reqs[0].method != http.MethodGet {
		t.Fatalf("requests %+v", reqs)
	}
	out, err = clitest.Exec(t, stubRoot(&stubBackend{runner: proc.NewFakeRunner()}), "msg", "list", "--json")
	if err != nil || !strings.Contains(out, `"messages":[{"id":"ab"`) {
		t.Fatalf("json %q: %v", out, err)
	}
}
