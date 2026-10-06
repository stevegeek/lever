package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// filesApp is a landing-chat app with one worker and its state directory
// outside the tree, or, with inside, in it.
func filesApp(t *testing.T, inside bool) (*config.App, state.State) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "workspace")
	st := state.ForConfig(root)
	if inside {
		tree = root
	}
	for _, d := range []string{tree, st.Dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &config.App{Name: "boss", Tree: tree, Workers: []config.Worker{{Name: "w1", Dir: "workers/w1"}},
		Remote: config.Remote{Enabled: true, BaseURL: "https://mac.ts.net", Landing: config.RemoteLandingChat,
			AllowedUsers: []config.RemoteUser{{Login: "op@x"}, {Login: "c@x", Tier: config.TierContact, Agents: []string{"w1"}}}}}, st
}

func TestCheckFiles(t *testing.T) {
	app, st := filesApp(t, false)
	if r := checkFiles(app, st); !r.ok || !strings.Contains(r.detail, "off") {
		t.Fatalf("off: %+v", r)
	}
	app.Remote.Files.Enabled = true
	r := checkFiles(app, st)
	if !r.ok || !strings.Contains(r.detail, "max 25 MiB") || !strings.Contains(r.detail, "contact_files, share_file") {
		t.Fatalf("on: %+v", r)
	}
	if err := os.MkdirAll(st.FilesLedger(), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(st.FilesLedger(), "w1.jsonl"), []byte("{}\n"), 0o644)
	_ = os.Chmod(filepath.Join(st.FilesLedger(), "w1.jsonl"), 0o666)
	if r := checkFiles(app, st); r.ok || !strings.Contains(r.detail, "w1.jsonl") {
		t.Fatalf("unsafe file: %+v", r)
	}
	_ = os.Chmod(filepath.Join(st.FilesLedger(), "w1.jsonl"), 0o600)
	if r := checkFiles(app, st); !r.ok {
		t.Fatalf("fixed: %+v", r)
	}
	ws := filepath.Join(app.Tree, app.Workers[0].Dir)
	_ = os.MkdirAll(ws, 0o755)
	_ = os.Symlink(t.TempDir(), filepath.Join(ws, chatfiles.Dir))
	if r := checkFiles(app, st); r.ok || !strings.Contains(r.detail, "w1") || !strings.Contains(r.detail, "link") {
		t.Fatalf("linked exchange: %+v", r)
	}
}

func TestCheckFilesStateInsideTree(t *testing.T) {
	app, st := filesApp(t, true)
	app.Remote.Files.Enabled = true
	if r := checkFiles(app, st); r.ok {
		t.Fatalf("%+v", r)
	}
	if c := remoteFiles(app, st); c == nil || c.LedgerDir != "" {
		t.Fatalf("%+v", c)
	}
}

func TestRemoteFilesConfig(t *testing.T) {
	app, st := filesApp(t, false)
	if remoteFiles(app, st) != nil {
		t.Fatal("off")
	}
	app.Remote.Files.Enabled = true
	c := remoteFiles(app, st)
	if c == nil || c.Tree != app.Tree || c.LedgerDir != st.FilesLedger() || c.Workspaces[app.Name] != "." ||
		c.Workspaces["w1"] != "workers/w1" || c.MaxBytes != 25<<20 || len(c.Extensions) == 0 {
		t.Fatalf("%+v", c)
	}
}

func TestCheckFilesUsageAndLinks(t *testing.T) {
	app, st := filesApp(t, false)
	app.Remote.Files.Enabled = true
	if r := checkFiles(app, st); !r.ok || !strings.Contains(r.detail, "no uploads stored") || !strings.Contains(r.detail, "never removes") {
		t.Fatalf("empty: %+v", r)
	}
	key := chatfiles.InDir("workers/w1", "c@x")
	if err := os.MkdirAll(filepath.Join(app.Tree, key), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(app.Tree, key, "a.pdf"), make([]byte, 2048), 0o600)
	_ = os.Symlink("/etc/hosts", filepath.Join(app.Tree, key, "l.pdf")) // a link leaf is not counted
	if r := checkFiles(app, st); !r.ok || !strings.Contains(r.detail, "w1 2.0 KiB") {
		t.Fatalf("usage: %+v", r)
	}
	for _, at := range []string{"workers/w1/.lever-files/in/" + chatfiles.Key("op@x"), "workers/w1/.lever-files/in/kunknown", "workers/w1/.lever-files/out"} {
		t.Run(at, func(t *testing.T) {
			if err := os.Symlink(t.TempDir(), filepath.Join(app.Tree, at)); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(filepath.Join(app.Tree, at))
			r := checkFiles(app, st)
			if r.ok || !strings.Contains(r.detail, at) {
				t.Fatalf("%+v", r)
			}
			want := "uploads from op@x to w1 are refused"
			switch {
			case strings.HasSuffix(at, "/out"):
				want = "every file w1 shares is refused"
			case strings.HasSuffix(at, "/kunknown"):
				want = "not a login's directory"
			}
			if !strings.Contains(r.detail, want) {
				t.Fatalf("want %q: %+v", want, r)
			}
		})
	}
	_ = os.MkdirAll(st.FilesLedger(), 0o700)
	_ = os.WriteFile(filepath.Join(st.FilesLedger(), ".lock"), nil, 0o600)
	_ = os.Chmod(filepath.Join(st.FilesLedger(), ".lock"), 0o666)
	if r := checkFiles(app, st); r.ok || !strings.Contains(r.detail, ".lock") {
		t.Fatalf("lock: %+v", r)
	}
}

func TestRemoteFilesSwitches(t *testing.T) {
	app, st := filesApp(t, false)
	app.Remote.Files.Enabled = true
	if c := remoteFiles(app, st); c.NoUploads || c.NoShares || len(c.Excluded) != 0 {
		t.Fatalf("defaults: %+v", c)
	}
	no := false
	app.Remote.Files.Uploads, app.Remote.Files.Shares = &no, &no
	app.Remote.AllowedUsers[1].Files = &no
	if c := remoteFiles(app, st); !c.NoUploads || !c.NoShares || len(c.Excluded) != 1 || c.Excluded[0] != "c@x" {
		t.Fatalf("%+v", c)
	}
}

func TestCheckFilesShowsTheSwitches(t *testing.T) {
	app, st := filesApp(t, false)
	app.Remote.Files.Enabled = true
	if r := checkFiles(app, st); !r.ok || !strings.Contains(r.detail, "uploads on, shares on") || strings.Contains(r.detail, "no files for") {
		t.Fatalf("defaults: %+v", r)
	}
	no := false
	app.Remote.Files.Shares = &no
	app.Remote.AllowedUsers[1].Files = &no
	if r := checkFiles(app, st); !r.ok || !strings.Contains(r.detail, "uploads on, shares off") || !strings.Contains(r.detail, "no files for c@x") {
		t.Fatalf("%+v", r)
	}
}
