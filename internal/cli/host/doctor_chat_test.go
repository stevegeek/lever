package host

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func verifiedChatFixture(t *testing.T, users ...string) (*config.App, state.State) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	st := state.ForConfig(root)
	for _, d := range []string{tree, st.Dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &config.App{Tree: tree, Remote: config.Remote{Enabled: true, AllowedUsers: remoteUsers(users...)}}, st
}

func TestCheckVerifiedChat(t *testing.T) {
	t.Run("remote off", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		app.Remote.Enabled = false
		if r := checkVerifiedChat(app, st); !r.ok || r.fix != "" {
			t.Fatalf("row = %+v, want a plain pass", r)
		}
	})
	t.Run("no allowed_users warns", func(t *testing.T) {
		app, st := verifiedChatFixture(t)
		if r := checkVerifiedChat(app, st); !r.ok || r.fix == "" || !strings.Contains(r.detail, "allowed_users") {
			t.Fatalf("row = %+v, want a warning naming allowed_users", r)
		}
	})
	t.Run("state inside the tree fails", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		app.Tree = filepath.Dir(st.Dir)
		if r := checkVerifiedChat(app, st); r.ok {
			t.Fatalf("row = %+v, want a failure", r)
		}
	})
	t.Run("on, nothing recorded yet", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		if r := checkVerifiedChat(app, st); !r.ok || r.fix != "" || !strings.Contains(r.detail, "no chat post") {
			t.Fatalf("row = %+v", r)
		}
	})
	t.Run("on, ledger 0700", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		if err := os.Mkdir(st.ChatLedger(), 0o700); err != nil {
			t.Fatal(err)
		}
		if r := checkVerifiedChat(app, st); !r.ok || r.fix != "" {
			t.Fatalf("row = %+v, want a pass", r)
		}
	})
	t.Run("writable ledger fails", func(t *testing.T) {
		app, st := verifiedChatFixture(t, "op@example.com")
		if err := os.Mkdir(st.ChatLedger(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(st.ChatLedger(), 0o777); err != nil {
			t.Fatal(err)
		}
		if r := checkVerifiedChat(app, st); r.ok {
			t.Fatalf("row = %+v, want a failure", r)
		}
	})
}

func remoteUsers(logins ...string) []config.RemoteUser {
	out := make([]config.RemoteUser, len(logins))
	for i, l := range logins {
		out[i] = config.RemoteUser{Login: l}
	}
	return out
}

func TestCheckChatLabels(t *testing.T) {
	tree := t.TempDir()
	app := &config.App{Name: "boss", Tree: tree, Workers: []config.Worker{{Name: "w1"}},
		Remote: config.Remote{Enabled: true, LabelsFile: "labels.json"}}
	p := filepath.Join(tree, "labels.json")
	write := func(b []byte) func() {
		return func() {
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, tc := range map[string]struct {
		setup    func()
		warn     bool
		contains string
	}{
		"absent":    {func() {}, false, "absent"},
		"good":      {write([]byte(`{"w1":"a","boss":"b","zz":"c"}`)), false, "2 label(s) for known agents in labels.json (1 other name(s) ignored)"},
		"not json":  {write([]byte(`[1]`)), true, "is not a JSON object of strings"},
		"too big":   {write(bytes.Repeat([]byte(" "), 16<<10+1)), true, "is larger than 16 KiB"},
		"symlink":   {func() { _ = os.Symlink("/etc/hosts", p) }, true, "symbolic link"},
		"directory": {func() { _ = os.Mkdir(p, 0o755) }, true, "is not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.RemoveAll(p)
			tc.setup()
			r := checkChatLabels(app)
			// A bad file is a warning: a passing row that carries a fix.
			if !r.ok || (r.fix != "") != tc.warn || !strings.Contains(r.detail, tc.contains) || r.name != "chat labels" {
				t.Fatalf("%+v", r)
			}
		})
	}
	app.Remote.LabelsFile = ""
	if r := checkChatLabels(app); !r.ok || !strings.Contains(r.detail, "not set") {
		t.Fatalf("unset: %+v", r)
	}
}

// The row names each contact's reach: the agents it messages and those it
// may only see.
func TestCheckVerifiedChatNamesSeeLists(t *testing.T) {
	app, st := verifiedChatFixture(t, "op@example.com")
	app.Remote.AllowedUsers = append(app.Remote.AllowedUsers,
		config.RemoteUser{Login: "c@x", Tier: config.TierContact, Agents: []string{"w1", "w3"}, See: []string{"w2"}},
		config.RemoteUser{Login: "d@x", Tier: config.TierContact, Agents: []string{"w4"}})
	r := checkVerifiedChat(app, st)
	if !strings.Contains(r.detail, "c@x contact (w1, w3; see: w2)") || !strings.Contains(r.detail, "d@x contact (w4)") {
		t.Fatalf("row = %+v", r)
	}
}

func TestCheckAgentMessages(t *testing.T) {
	tree := t.TempDir()
	st := state.State{Dir: t.TempDir()}
	app := &config.App{Name: "x", Tree: tree, Remote: config.Remote{Enabled: true,
		AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"x"}}}}}
	if r := checkAgentMessages(app, st, agentMsgsLive{}); !r.ok || !strings.Contains(r.detail, "off") {
		t.Fatalf("off: %+v", r)
	}
	app.Remote.AgentMessages.Enabled = true
	r := checkAgentMessages(app, st, agentMsgsLive{})
	if !r.ok || !strings.Contains(r.detail, "c@x (x)") || !strings.Contains(r.detail, "follow_up_after 24h,") ||
		!strings.Contains(r.detail, "max_chars 4000") || !strings.Contains(r.detail, "image") {
		t.Fatalf("on: %+v", r)
	}
	if strings.Contains(r.detail, "op@x") {
		t.Fatalf("an operator is not a contact: %+v", r)
	}
	app.Remote.AgentMessages.FollowUpAfter = 90 * time.Minute
	if r := checkAgentMessages(app, st, agentMsgsLive{}); !strings.Contains(r.detail, "follow_up_after 1h30m0s") {
		t.Fatalf("a duration that is not whole hours: %+v", r)
	}
	if err := os.MkdirAll(st.AgentLedger(), 0o700); err != nil {
		t.Fatal(err)
	}
	if r := checkAgentMessages(app, st, agentMsgsLive{}); !r.ok || !strings.Contains(r.detail, "0700") {
		t.Fatalf("a private ledger: %+v", r)
	}
	if err := os.Chmod(st.AgentLedger(), 0o777); err != nil {
		t.Fatal(err)
	}
	if r := checkAgentMessages(app, st, agentMsgsLive{}); r.ok {
		t.Fatalf("a group-writable ledger must fail: %+v", r)
	}
	inside := state.State{Dir: filepath.Join(tree, ".lever-state")}
	if r := checkAgentMessages(app, inside, agentMsgsLive{}); r.ok {
		t.Fatalf("state inside the tree must fail: %+v", r)
	}
}

// The row says when the running broker or proxy started with another
// config while the setting is on; while it is off, only when a process runs
// with agent messages on (the setting itself differs).
func TestCheckAgentMessagesNamesAStaleProcess(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	users := []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"x"}}}
	off := &config.App{Name: "x", Tree: t.TempDir(), Remote: config.Remote{Enabled: true, AllowedUsers: users}}
	on := *off
	on.Remote.AgentMessages.Enabled = true
	other := *off
	other.Workers = []config.Worker{{Name: "w9"}} // another config, setting off
	broker := func(a *config.App) func() (string, bool) {
		return func() (string, bool) { return brokerctl.ConfigHash(a), true }
	}
	proxy := func(a *config.App) func(string) (bool, bool) {
		return func(h string) (bool, bool) { return true, h == brokerctl.RemoteConfigHash(a) }
	}
	none := func() (string, bool) { return "", false }
	noProxy := func(string) (bool, bool) { return false, false }
	for name, tc := range map[string]struct {
		cfg   *config.App
		live  agentMsgsLive
		stale string
	}{
		"on, current":            {&on, agentMsgsLive{broker(&on), proxy(&on), nil}, ""},
		"on, nothing runs":       {&on, agentMsgsLive{none, noProxy, nil}, ""},
		"on, both started off":   {&on, agentMsgsLive{broker(off), proxy(off), nil}, "broker and remote proxy"},
		"on, proxy started off":  {&on, agentMsgsLive{broker(&on), proxy(off), nil}, "running remote proxy started"},
		"off, current":           {off, agentMsgsLive{broker(off), proxy(off), nil}, ""},
		"off, both started on":   {off, agentMsgsLive{broker(&on), proxy(&on), nil}, "broker and remote proxy"},
		"off, other config":      {off, agentMsgsLive{broker(&other), proxy(&other), nil}, ""},
		"off, broker started on": {off, agentMsgsLive{broker(&on), proxy(off), nil}, "running broker started"},
	} {
		r := checkAgentMessages(tc.cfg, st, tc.live)
		if !r.ok || (tc.stale == "") != (r.fix == "") || !strings.Contains(r.detail, tc.stale) {
			t.Errorf("%s: %+v", name, r)
		}
		if tc.stale != "" && !strings.Contains(r.detail, "`lever init` + `lever apply`") {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

// One contact file another user could write refuses every authorization;
// the row says so.
func TestCheckAgentMessagesUnsafeContactFile(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	app := &config.App{Name: "x", Tree: t.TempDir(), Remote: config.Remote{Enabled: true, AgentMessages: config.AgentMessages{Enabled: true},
		AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"x"}}}}}
	if err := os.MkdirAll(st.AgentLedger(), 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(st.AgentLedger(), "c-000000000000000000000000.jsonl")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkAgentMessages(app, st, agentMsgsLive{}); !r.ok {
		t.Fatalf("a private file: %+v", r)
	}
	if err := os.Chmod(f, 0o666); err != nil {
		t.Fatal(err)
	}
	if r := checkAgentMessages(app, st, agentMsgsLive{}); r.ok || !strings.Contains(r.detail, "every agent's contact_message is refused") {
		t.Fatalf("an unsafe file: %+v", r)
	}
	// A rotated .1 counts too; a file the ledger does not read does not.
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(st.AgentLedger(), "c-notes.txt")
	if err := os.WriteFile(stray, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(stray, 0o666)
	if r := checkAgentMessages(app, st, agentMsgsLive{}); !r.ok {
		t.Fatalf("a stray file: %+v", r)
	}
	if err := os.WriteFile(f+".1", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(f+".1", 0o666)
	if r := checkAgentMessages(app, st, agentMsgsLive{}); r.ok {
		t.Fatalf("an unsafe .1: %+v", r)
	}
}

// The filter trusts the hub's sender, which scion sets itself only from
// b3562fb1: the row fails on a scion known to be older and notes one it
// cannot check.
func TestCheckAgentMessagesScionFloor(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	base := func() *config.App {
		return &config.App{Name: "x", Tree: t.TempDir(), Remote: config.Remote{Enabled: true, AgentMessages: config.AgentMessages{Enabled: true},
			AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"x"}}}}}
	}
	has := func(ok bool, err error) func(string, string) (bool, error) {
		return func(_, commit string) (bool, error) {
			if commit != config.AgentMessagesScionFloor {
				t.Fatalf("commit = %s", commit)
			}
			return ok, err
		}
	}
	for name, tc := range map[string]struct {
		scion    config.ScionConfig
		has      func(string, string) (bool, error)
		ok, note bool
		want     string
	}{
		"version at the fix":     {config.ScionConfig{Version: "v0.0.0-20260828114021-b3562fb19a97"}, nil, true, false, ""},
		"version before the fix": {config.ScionConfig{Version: "v0.0.0-20260801000000-0123456789ab"}, nil, false, false, "predates b3562fb1"},
		"version bare hash":      {config.ScionConfig{Version: "63d5d65d"}, nil, true, true, "cannot tell"},
		"source with the fix":    {config.ScionConfig{Source: "/s"}, has(true, nil), true, false, ""},
		"source before the fix":  {config.ScionConfig{Source: "/s"}, has(false, nil), false, false, "checkout predates b3562fb1"},
		"source git cannot tell": {config.ScionConfig{Source: "/s"}, has(false, errors.New("not a git repository")), true, true, "not a git repository"},
		"binary":                 {config.ScionConfig{Binary: "/b"}, nil, true, true, "scion.binary"},
	} {
		app := base()
		app.Scion = tc.scion
		r := checkAgentMessages(app, st, agentMsgsLive{scionHasCommit: tc.has})
		if r.ok != tc.ok || tc.note != (r.fix != "" && r.ok) || !strings.Contains(r.detail, tc.want) {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// Off: nothing to check.
	app := base()
	app.Remote.AgentMessages.Enabled = false
	app.Scion.Version = "v0.0.0-20260801000000-0123456789ab"
	if r := checkAgentMessages(app, st, agentMsgsLive{}); !r.ok || r.fix != "" {
		t.Fatalf("off: %+v", r)
	}
}

// Without remote access the setting cannot be on: the row does not probe
// the broker.
func TestCheckAgentMessagesSkipsTheBrokerProbeWhenItCannotBeOn(t *testing.T) {
	st := state.State{Dir: t.TempDir()}
	app := &config.App{Name: "x", Tree: t.TempDir()}
	probed := false
	live := agentMsgsLive{brokerHash: func() (string, bool) { probed = true; return "", false }}
	if r := checkAgentMessages(app, st, live); !r.ok || probed {
		t.Fatalf("remote off: probed=%v %+v", probed, r)
	}
	app.Remote = config.Remote{Enabled: true, AllowedUsers: []config.RemoteUser{{Login: "op@x"}}}
	if checkAgentMessages(app, st, live); !probed {
		t.Fatal("remote on, setting off: a broker started with it on must still be found")
	}
}
