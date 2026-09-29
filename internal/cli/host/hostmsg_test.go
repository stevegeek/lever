package host

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/testutil"
	"github.com/stevegeek/lever/internal/wire"
)

// startOperatorUDS serves canned answers on <dir>/.lever-state/operator.sock,
// the socket `lever msg send` dials, and records every request.
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

// TestHostMsgSendPostsToTheOperatorSocket: the note goes to the broker's
// operator socket with the recipient, the joined body and the interrupt flag,
// and never to scion directly.
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
