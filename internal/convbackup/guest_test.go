package convbackup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
)

// These tests run the guest scripts with the real bash, GNU tar and
// coreutils of the machine running the tests, in a fake HOME laid out the
// way scion lays out the jail run user's. They are what shows the scripts
// never follow a link the agent planted.

func needGNU(t *testing.T) {
	t.Helper()
	out, err := exec.Command("tar", "--version").Output()
	if err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("needs GNU tar")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	if out, err := exec.Command("du", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU") {
		t.Skip("needs GNU coreutils")
	}
}

// fakeJail makes HOME with one scion registration for /lever and the agent
// home of "mgr", and returns HOME and the agent home.
func fakeJail(t *testing.T) (home, agentHome string) {
	t.Helper()
	needGNU(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	entry := filepath.Join(home, ".scion", "project-configs", "lever__abc", ".scion")
	agentHome = filepath.Join(entry, "agents", "mgr", "home")
	if err := os.MkdirAll(agentHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(entry, "settings.yaml"), "workspace_path: /lever\n")
	return home, agentHome
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func guest() Guest { return Guest{R: proc.RealRunner{}, Workspace: "/lever", Agent: "mgr"} }

// TestCaptureAndInstallRoundTrip: what is captured from one home comes back
// in another with its times, the transcripts there are moved aside, and a
// link inside the projects never carries what it points at.
func TestCaptureAndInstallRoundTrip(t *testing.T) {
	home, ah := fakeJail(t)
	secret := filepath.Join(home, "secret")
	writeFile(t, secret, "SECRET")
	old := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	tr := filepath.Join(ah, ".claude", "projects", "-workspace", "old-session.jsonl")
	writeFile(t, tr, "{\"old\":true}\n")
	if err := os.Chtimes(tr, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(ah, ".claude", "projects", "-workspace", "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ah, ".claude", ".credentials.json"), "TOKEN")
	writeFile(t, filepath.Join(ah, ".claude.json"), "{}")

	raw, found, _, err := guest().Capture(context.Background())
	if err != nil || !found {
		t.Fatalf("capture: found=%v err=%v", found, err)
	}
	var stored bytes.Buffer
	st, err := Copy(&stored, bytes.NewReader(raw), "projects", DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 1 || st.Skipped != 1 || st.Newest != "-workspace/old-session.jsonl" {
		t.Fatalf("stats = %+v", st)
	}
	for name := range readTar(t, stored.Bytes()) {
		if strings.Contains(name, "credentials") || strings.Contains(name, "claude.json") {
			t.Fatalf("captured %s", name)
		}
	}

	// The fresh record's home: a new session, and the agent planted a link
	// named like the restore's own directory.
	if err := os.RemoveAll(filepath.Join(ah, ".claude", "projects")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(ah, ".claude", "projects", "-workspace", "fresh.jsonl"), "fresh\n")
	parked, err := guest().Install(context.Background(), bytes.NewReader(stored.Bytes()), "20261012T100000Z")
	if err != nil {
		t.Fatal(err)
	}
	if parked != ".claude/projects.lever-replaced-20261012T100000Z" {
		t.Fatalf("parked = %q", parked)
	}
	if b, err := os.ReadFile(filepath.Join(ah, parked, "-workspace", "fresh.jsonl")); err != nil || string(b) != "fresh\n" {
		t.Fatalf("parked session: %q %v", b, err)
	}
	fi, err := os.Stat(tr)
	if err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("restored transcript: %v %v", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(ah, ".claude", "projects", "-workspace", "link.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("the link came back: %v", err)
	}
	ents, _ := os.ReadDir(filepath.Join(ah, ".claude"))
	for _, e := range ents {
		if strings.Contains(e.Name(), "restoring") {
			t.Fatalf("left %s", e.Name())
		}
	}
}

// TestCaptureNothingToBackUp: no registration, no home, or no projects is
// "not found", never an error.
func TestCaptureNothingToBackUp(t *testing.T) {
	_, ah := fakeJail(t)
	if _, found, _, err := guest().Capture(context.Background()); err != nil || found {
		t.Fatalf("no .claude: found=%v err=%v", found, err)
	}
	if err := os.Mkdir(filepath.Join(ah, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, found, _, err := guest().Capture(context.Background()); err != nil || found {
		t.Fatalf("no projects: found=%v err=%v", found, err)
	}
	g := guest()
	g.Agent = "other"
	if _, found, _, err := g.Capture(context.Background()); err != nil || found {
		t.Fatalf("no home: found=%v err=%v", found, err)
	}
	g = guest()
	g.Workspace = "/elsewhere"
	if _, found, _, err := g.Capture(context.Background()); err != nil || found {
		t.Fatalf("no registration: found=%v err=%v", found, err)
	}
}

// TestCaptureRefusesTwoRegistrations: two registrations for the workspace
// leave the home ambiguous, and the capture fails rather than guess.
func TestCaptureRefusesTwoRegistrations(t *testing.T) {
	home, ah := fakeJail(t)
	writeFile(t, filepath.Join(ah, ".claude", "projects", "-workspace", "a.jsonl"), "x\n")
	writeFile(t, filepath.Join(home, ".scion", "project-configs", "lever__dup", ".scion", "settings.yaml"), "workspace_path: /lever\n")
	if _, _, _, err := guest().Capture(context.Background()); err == nil || !strings.Contains(err.Error(), "registrations") {
		t.Fatalf("err = %v", err)
	}
}

// TestCaptureRefusesLinkedDirs: a .claude or a projects the agent replaced
// with a link to a directory outside its home is never read.
func TestCaptureRefusesLinkedDirs(t *testing.T) {
	home, ah := fakeJail(t)
	outside := filepath.Join(home, "outside")
	writeFile(t, filepath.Join(outside, "projects", "x.jsonl"), "guest secret\n")
	if err := os.Symlink(outside, filepath.Join(ah, ".claude")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := guest().Capture(context.Background()); !errors.Is(err, ErrNotPlainDir) {
		t.Fatalf(".claude link: %v", err)
	}
	if err := os.Remove(filepath.Join(ah, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ah, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "projects"), filepath.Join(ah, ".claude", "projects")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := guest().Capture(context.Background()); !errors.Is(err, ErrNotPlainDir) {
		t.Fatalf("projects link: %v", err)
	}
}

// TestInstallNeverWritesThroughALink: a .claude linked out of the home is
// refused and the directory it points at is untouched; a projects link is
// moved aside as a link, and its target keeps its files.
func TestInstallNeverWritesThroughALink(t *testing.T) {
	home, ah := fakeJail(t)
	stored := mkTar(t, entry{name: "-workspace/a.jsonl", typ: '0', body: "restored\n"})
	outside := filepath.Join(home, "outside")
	writeFile(t, filepath.Join(outside, "keep"), "k")
	if err := os.Symlink(outside, filepath.Join(ah, ".claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := guest().Install(context.Background(), bytes.NewReader(stored), "s1"); !errors.Is(err, ErrNotPlainDir) {
		t.Fatalf(".claude link: %v", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("wrote through the link: %v", ents)
	}

	if err := os.Remove(filepath.Join(ah, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ah, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ah, ".claude", "projects")); err != nil {
		t.Fatal(err)
	}
	parked, err := guest().Install(context.Background(), bytes.NewReader(stored), "s2")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(ah, parked)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("parked link: %v %v", fi, err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("the link's target changed: %v", ents)
	}
	if b, err := os.ReadFile(filepath.Join(ah, ".claude", "projects", "-workspace", "a.jsonl")); err != nil || string(b) != "restored\n" {
		t.Fatalf("restored: %q %v", b, err)
	}
}

// TestInstallBadArchiveKeepsCurrent: an archive that does not unpack leaves
// the current projects in place and no half-written directory.
func TestInstallBadArchiveKeepsCurrent(t *testing.T) {
	_, ah := fakeJail(t)
	writeFile(t, filepath.Join(ah, ".claude", "projects", "-workspace", "cur.jsonl"), "cur\n")
	if _, err := guest().Install(context.Background(), strings.NewReader("garbage"), "s3"); err == nil {
		t.Fatal("want an error")
	}
	if b, err := os.ReadFile(filepath.Join(ah, ".claude", "projects", "-workspace", "cur.jsonl")); err != nil || string(b) != "cur\n" {
		t.Fatalf("current: %q %v", b, err)
	}
	ents, _ := os.ReadDir(filepath.Join(ah, ".claude"))
	if len(ents) != 1 {
		t.Fatalf(".claude holds %v", ents)
	}
	if _, err := guest().Install(context.Background(), strings.NewReader(""), "../x"); err == nil {
		t.Fatal("a bad stamp was accepted")
	}
}
