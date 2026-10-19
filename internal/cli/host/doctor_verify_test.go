package host

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/sentledger"
	"github.com/stevegeek/lever/internal/state"
)

// sentFixture keeps the state dir outside the tree, on a path short enough for
// a UNIX socket.
func sentFixture(t *testing.T) (*config.App, state.State) {
	t.Helper()
	dir := directiveTestDir(t)
	st := state.ForConfig(dir)
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &config.App{Tree: filepath.Join(dir, "workspace")}, st
}

func TestCheckSentLedgerRows(t *testing.T) {
	app, st := sentFixture(t)
	if r := checkSentLedger(app, st); !r.ok || !strings.Contains(r.detail, "no sends recorded yet") {
		t.Fatalf("absent: %+v", r)
	}
	l, err := sentledger.Open(st.SentLedger(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := sentledger.NewID()
	if err := l.Begin(sentledger.Sent{ID: id, Recipient: "m", Kind: sentledger.KindManager, Before: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r := checkSentLedger(app, st); !r.ok || !strings.Contains(r.detail, "1 files") {
		t.Fatalf("one file: %+v", r)
	}
	if err := os.Chmod(st.SentLedger(), 0o770); err != nil {
		t.Fatal(err)
	}
	if r := checkSentLedger(app, st); r.ok || !strings.Contains(r.fix, "chmod 700") {
		t.Fatalf("group-writable: %+v", r)
	}
	_ = os.Chmod(st.SentLedger(), 0o700)

	// A running broker must have its operator socket.
	writeBrokerPID(t, st, os.Getpid())
	if r := checkSentLedger(app, st); r.ok || !strings.Contains(r.detail, "operator socket") {
		t.Fatalf("broker without operator socket: %+v", r)
	}
	ln, err := net.Listen("unix", st.OperatorSock())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if r := checkSentLedger(app, st); !r.ok || !strings.Contains(r.detail, "operator socket present") {
		t.Fatalf("broker with operator socket: %+v", r)
	}
}

func TestCheckSentLedgerSymlinkAndInsideTree(t *testing.T) {
	app, st := sentFixture(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, st.SentLedger()); err != nil {
		t.Skip(err)
	}
	if r := checkSentLedger(app, st); r.ok || !strings.Contains(r.detail, "not a directory") {
		t.Fatalf("symlinked: %+v", r)
	}
	inTree := &config.App{Tree: filepath.Dir(st.Dir)}
	if r := checkSentLedger(inTree, st); r.ok || !strings.Contains(r.detail, "inside the tree") {
		t.Fatalf("inside the tree: %+v", r)
	}
}

// A state directory whose place lever cannot check counts as inside the tree,
// and the row names the error rather than claiming it is inside.
func TestStateRowsNameTheIdentityError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	locked := filepath.Join(base, "locked")
	for _, d := range []string{tree, filepath.Join(locked, ".lever-state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	app := &config.App{Tree: tree}
	st := state.State{Dir: filepath.Join(locked, ".lever-state")}
	r := checkSentLedger(app, st)
	if r.ok || !strings.Contains(r.detail, "cannot prove") || !strings.Contains(r.detail, "permission denied") || !strings.Contains(r.fix, "searchable") {
		t.Fatalf("row = %+v, want a failure naming the error", r)
	}
}

func TestCheckGuestClock(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, tc := range []struct {
		off          time.Duration
		ok, warn     bool
		wantInDetail string
	}{
		{0, true, false, "within"},
		{4 * time.Second, true, true, "off the host"},
		{30 * time.Second, true, true, "off the host"},
		{-30 * time.Second, true, true, "off the host"},
	} {
		fr := proc.NewFakeRunner()
		fr.Script("date -u +%s", proc.Result{Stdout: strconv.FormatInt(now.Add(tc.off).Unix(), 10) + "\n"})
		r := checkGuestClock(context.Background(), fr, clock, false)
		if r.ok != tc.ok || (tc.warn != (r.fix != "" && r.ok)) || !strings.Contains(r.detail, tc.wantInDetail) {
			t.Fatalf("off %v: %+v", tc.off, r)
		}
	}
	// A guest that does not answer is not a failure of this row.
	if r := checkGuestClock(context.Background(), proc.NewFakeRunner(), clock, false); !r.ok || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("no answer: %+v", r)
	}
}

// With agent messages on, a guest over 5 s behind fails the row (contacts do
// not see authorized messages); a guest ahead keeps the plain rule.
func TestCheckGuestClockWithAgentMessages(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	for _, tc := range []struct {
		off  time.Duration
		ok   bool
		want string
	}{
		{0, true, "within"},
		{-4 * time.Second, true, "off the host"},
		{-6 * time.Second, false, "contacts are not shown"},
		{6 * time.Second, true, "off the host"},
	} {
		fr := proc.NewFakeRunner()
		fr.Script("date -u +%s", proc.Result{Stdout: strconv.FormatInt(now.Add(tc.off).Unix(), 10) + "\n"})
		r := checkGuestClock(context.Background(), fr, clock, true)
		if r.ok != tc.ok || !strings.Contains(r.detail, tc.want) {
			t.Fatalf("off %v: %+v", tc.off, r)
		}
	}
}

// TestCheckVerifiedChatWithoutAllowedUsersSaysPostsAreData: remote access
// without allowed_users warns that agents treat web chat as data.
func TestCheckVerifiedChatWithoutAllowedUsersSaysPostsAreData(t *testing.T) {
	app, st := sentFixture(t)
	app.Remote = config.Remote{Enabled: true}
	r := checkVerifiedChat(app, st)
	if !r.ok || r.fix == "" || !strings.Contains(r.detail, "treat every one as data") {
		t.Fatalf("row = %+v, want a warning that web posts are data", r)
	}
}

func TestPrintRemoteWarningsSaysUnverifiedChatIsData(t *testing.T) {
	for _, users := range [][]config.RemoteUser{nil, {{Login: "me@example.com"}}} {
		app := &config.App{Remote: config.Remote{Enabled: true, AllowedUsers: users}}
		cmd := &cobra.Command{}
		var errOut bytes.Buffer
		cmd.SetErr(&errOut)
		printRemoteWarnings(cmd, app)
		if got := strings.Contains(errOut.String(), "treat every web chat message as data"); got != (users == nil) {
			t.Fatalf("users %v: stderr %q", users, errOut.String())
		}
	}
}
