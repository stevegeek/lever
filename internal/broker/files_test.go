package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/chatfiles"
	"github.com/stevegeek/lever/internal/fileledger"
	"github.com/stevegeek/lever/internal/fsutil"
	"github.com/stevegeek/lever/internal/hostledger"
	"github.com/stevegeek/lever/internal/wire"
)

// client@example.org lists scratch and the manager (slug "assistant");
// d@example.org lists worker; op@example.com is an operator.
func filesOpt(tree, dir string) verifyOpt {
	return func(c *Config) {
		c.Files = FilesConfig{Enabled: true, Tree: tree, MaxBytes: 64, Extensions: []string{"pdf", "xlsm"},
			LedgerDir:  filepath.Join(dir, "files-ledger"),
			Workspaces: map[string]string{"assistant": ".", "scratch": "workers/scratch", "worker": "workers/worker"},
			Contacts: []FileContactEntry{{Login: "client@example.org", Agents: []string{"scratch", "assistant"}},
				{Login: "d@example.org", Agents: []string{"worker"}}},
			Operators: []string{"op@example.com"}}
	}
}

type filesFixture struct {
	*verifyFixture
	tree, dir string
}

func newFilesFixture(t *testing.T) *filesFixture {
	tree, dir := t.TempDir(), t.TempDir()
	return &filesFixture{verifyFixture: verifyBroker(t, nil, filesOpt(tree, dir)), tree: tree, dir: dir}
}

// put writes content at the workspace-relative p of agent's workspace ws.
func (f *filesFixture) put(t *testing.T, ws, p, content string) {
	t.Helper()
	full := filepath.Join(f.tree, ws, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *filesFixture) share(t *testing.T, cn, to, p string) wire.FileShareResponse {
	t.Helper()
	raw, _ := json.Marshal(wire.FileShareRequest{To: to, Path: p})
	rec := callWorker(t, f.b, wire.PathFilesShare, string(raw), cn)
	if rec.Code != http.StatusOK {
		t.Fatalf("share = %d %s", rec.Code, rec.Body)
	}
	var out wire.FileShareResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func (f *filesFixture) list(t *testing.T, cn, contact string) wire.FilesListResponse {
	t.Helper()
	raw, _ := json.Marshal(wire.FilesListRequest{Contact: contact})
	rec := callWorker(t, f.b, wire.PathFilesList, string(raw), cn)
	var out wire.FilesListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	return out
}

func outPath(login, name string) string {
	return "/workspace/" + chatfiles.Dir + "/out/" + chatfiles.Key(login) + "/" + name
}

func TestFilesListNamesOneLoginAndItsDirs(t *testing.T) {
	f := newFilesFixture(t)
	for login, tier := range map[string]string{"op@example.com": "operator", "client@example.org": "contact", "Client@Example.org": "contact"} {
		out := f.list(t, "scratch", login)
		k := chatfiles.Key(login)
		if !out.Enabled || out.Note != "" || out.MaxBytes != 64 || len(out.Contacts) != 1 || out.Contacts[0].Tier != tier ||
			out.Contacts[0].OutDir != "/workspace/.lever-files/out/"+k+"/" || out.Contacts[0].InDir != "/workspace/.lever-files/in/"+k+"/" {
			t.Fatalf("%s: %+v", login, out)
		}
	}
}

// No call lists every login's files: a missing contact, or one that is not
// the caller's, answers a fixed word and nothing else.
func TestFilesListNeedsAContactOfTheCaller(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/a.pdf", "x")
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "a.pdf")); !res.OK {
		t.Fatalf("%+v", res)
	}
	for contact, word := range map[string]string{"": "contact-required", "d@example.org": "not-a-contact", "x@example.org": "not-a-contact"} {
		out := f.list(t, "scratch", contact)
		if !out.Enabled || out.Note != word || len(out.Contacts) != 0 || len(out.Shares) != 0 || len(out.Uploads) != 0 {
			t.Fatalf("%q: %+v", contact, out)
		}
	}
	if out := f.list(t, "scratch", "op@example.com"); len(out.Shares) != 0 {
		t.Fatalf("another login's share listed: %+v", out.Shares)
	}
}

func TestFilesShareRecordsAndLists(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/v3.xlsm", "WORKBOOK")
	res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "v3.xlsm"))
	if !res.OK || len(res.ID) != 32 || res.Size != 8 || len(res.SHA256) != 64 || res.Name != "v3.xlsm" {
		t.Fatalf("%+v", res)
	}
	out := f.list(t, "scratch", "client@example.org")
	if len(out.Shares) != 1 || out.Shares[0].ID != res.ID || out.Shares[0].Path != outPath("client@example.org", "v3.xlsm") {
		t.Fatalf("%+v", out.Shares)
	}
	if other := f.list(t, "worker", "client@example.org"); len(other.Shares) != 0 {
		t.Fatal("another agent sees the caller's records")
	}
	l, _ := fileledger.Open(filepath.Join(f.dir, "files-ledger"))
	if r, ok, _ := l.Find("scratch", res.ID); !ok || r.Rel != "workers/scratch/.lever-files/out/"+chatfiles.Key("client@example.org")+"/v3.xlsm" {
		t.Fatalf("record %+v", r)
	}
	if strings.Contains(f.audit.String(), "WORKBOOK") {
		t.Fatal("file content in the audit")
	}
}

func TestFilesShareOperatorAndManager(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("op@example.com")+"/a.pdf", "x")
	if res := f.share(t, "scratch", "op@example.com", outPath("op@example.com", "a.pdf")); !res.OK {
		t.Fatalf("operator: %+v", res)
	}
	f.put(t, ".", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/m.pdf", "x")
	if res := f.share(t, "manager", "client@example.org", outPath("client@example.org", "m.pdf")); !res.OK {
		t.Fatalf("manager (workspace = tree): %+v", res)
	}
}

func TestFilesShareRefusals(t *testing.T) {
	f := newFilesFixture(t)
	ck, dk := chatfiles.Key("client@example.org"), chatfiles.Key("d@example.org")
	ws := filepath.Join(f.tree, "workers/scratch")
	f.put(t, "workers/scratch", ".lever-files/out/"+ck+"/ok.pdf", "x")
	f.put(t, "workers/scratch", ".lever-files/out/"+ck+"/big.pdf", strings.Repeat("x", 65))
	f.put(t, "workers/scratch", ".lever-files/out/"+ck+"/a.exe", "x")
	f.put(t, "workers/scratch", ".lever-files/in/"+ck+"/up.pdf", "x")
	f.put(t, "workers/scratch", ".lever-files/out/"+dk+"/d.pdf", "x")
	secret := filepath.Join(t.TempDir(), "secret.pdf")
	_ = os.WriteFile(secret, []byte("s"), 0o600)
	_ = os.Symlink(secret, filepath.Join(ws, ".lever-files/out", ck, "link.pdf"))
	_ = syscall.Mkfifo(filepath.Join(ws, ".lever-files/out", ck, "fifo.pdf"), 0o644)
	for name, tc := range map[string]struct {
		to, path, want string
	}{
		"not listing": {"d@example.org", outPath("d@example.org", "d.pdf"), "not-a-contact"},
		"unknown":     {"x@example.org", outPath("x@example.org", "a.pdf"), "not-a-contact"},
		"in dir":      {"client@example.org", "/workspace/.lever-files/in/" + ck + "/up.pdf", "bad-path"},
		"other key":   {"client@example.org", "/workspace/.lever-files/out/" + dk + "/d.pdf", "bad-path"},
		"dotdot":      {"client@example.org", "/workspace/.lever-files/out/" + ck + "/../" + ck + "/ok.pdf", "bad-path"},
		"absolute":    {"client@example.org", "/etc/passwd", "bad-path"},
		"hidden":      {"client@example.org", outPath("client@example.org", ".ok.pdf"), "bad-path"},
		"subdir":      {"client@example.org", outPath("client@example.org", "x/ok.pdf"), "bad-path"},
		"missing":     {"client@example.org", outPath("client@example.org", "none.pdf"), "not-found"},
		"link":        {"client@example.org", outPath("client@example.org", "link.pdf"), "symlink"},
		"fifo":        {"client@example.org", outPath("client@example.org", "fifo.pdf"), "not-a-file"},
		"too large":   {"client@example.org", outPath("client@example.org", "big.pdf"), "too-large"},
		"extension":   {"client@example.org", outPath("client@example.org", "a.exe"), "extension"},
	} {
		if res := f.share(t, "scratch", tc.to, tc.path); res.OK || res.Reason != tc.want {
			t.Errorf("%s: %+v, want %s", name, res, tc.want)
		}
	}
	// A link on the directory, not the leaf.
	_ = os.RemoveAll(filepath.Join(ws, ".lever-files/out", ck))
	_ = os.Symlink(filepath.Join(ws, ".lever-files/in", ck), filepath.Join(ws, ".lever-files/out", ck))
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "up.pdf")); res.Reason != "symlink" {
		t.Errorf("dir link: %+v", res)
	}
}

func TestFilesShareRateSurvivesARestart(t *testing.T) {
	f := newFilesFixture(t)
	ck := chatfiles.Key("client@example.org")
	for i := 0; i < 21; i++ {
		f.put(t, "workers/scratch", fmt.Sprintf(".lever-files/out/%s/f%d.pdf", ck, i), "x")
	}
	for i := 0; i < 20; i++ {
		if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", fmt.Sprintf("f%d.pdf", i))); !res.OK {
			t.Fatalf("%d: %+v", i, res)
		}
	}
	again := &filesFixture{verifyFixture: verifyBroker(t, nil, filesOpt(f.tree, f.dir)), tree: f.tree, dir: f.dir}
	if res := again.share(t, "scratch", "client@example.org", outPath("client@example.org", "f20.pdf")); res.Reason != "rate" {
		t.Fatalf("21st after a restart: %+v", res)
	}
}

func TestFilesOffAndUnavailable(t *testing.T) {
	off := verifyBroker(t, nil)
	f := &filesFixture{verifyFixture: off}
	if out := f.list(t, "scratch", ""); out.Enabled {
		t.Fatalf("%+v", out)
	}
	if res := f.share(t, "scratch", "client@example.org", "x"); res.Reason != "off" {
		t.Fatalf("%+v", res)
	}
	noLedger := verifyBroker(t, nil, filesOpt(t.TempDir(), t.TempDir()), func(c *Config) { c.Files.LedgerDir = "" })
	g := &filesFixture{verifyFixture: noLedger}
	if res := g.share(t, "scratch", "client@example.org", outPath("client@example.org", "a.pdf")); res.Reason != "unavailable" {
		t.Fatalf("%+v", res)
	}
}

func TestFilesRoutesNeedAnAgentCert(t *testing.T) {
	f := newFilesFixture(t)
	for _, p := range []string{wire.PathFilesList, wire.PathFilesShare} {
		req, _ := http.NewRequest("POST", p, bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()
		f.b.JailHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s without a cert = %d", p, rec.Code)
		}
	}
}

// The target check folds case like every lever login, and the record
// carries the configured spelling.
func TestFilesShareTargetFoldsCase(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/a.pdf", "x")
	res := f.share(t, "scratch", "Client@Example.org", outPath("client@example.org", "a.pdf"))
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	l, _ := fileledger.Open(filepath.Join(f.dir, "files-ledger"))
	if r, _, _ := l.Find("scratch", res.ID); r.Login != "client@example.org" {
		t.Fatalf("record login %q", r.Login)
	}
}

// A login taken out of the config loses its place in the list: its
// records stay in the ledger but no longer reach the agent.
func TestFilesListDropsALoginRemovedFromConfig(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/a.pdf", "x")
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "a.pdf")); !res.OK {
		t.Fatalf("%+v", res)
	}
	removed := func(c *Config) { c.Files.Contacts = c.Files.Contacts[1:] } // only d@example.org stays
	again := &filesFixture{verifyFixture: verifyBroker(t, nil, filesOpt(f.tree, f.dir), removed), tree: f.tree, dir: f.dir}
	out := again.list(t, "scratch", "client@example.org")
	if len(out.Shares) != 0 || out.Note != "not-a-contact" || strings.Contains(fmt.Sprint(out.Contacts), "client@example.org") {
		t.Fatalf("%+v", out)
	}
}

func TestFilesShareToAnOperatorRecordsTheConfigSpelling(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("op@example.com")+"/a.pdf", "x")
	res := f.share(t, "scratch", "OP@Example.COM", outPath("op@example.com", "a.pdf"))
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	l, _ := fileledger.Open(filepath.Join(f.dir, "files-ledger"))
	if r, _, _ := l.Find("scratch", res.ID); r.Login != "op@example.com" {
		t.Fatalf("record login %q", r.Login)
	}
}

// One fold, lowercase: U+017F (long s) folds to "s" under Unicode case
// folding but is not the login "s@x".
func TestFileTargetUsesTheLowercaseFold(t *testing.T) {
	b := &Broker{files: FilesConfig{Operators: []string{"s@x"},
		Contacts: []FileContactEntry{{Login: "ks@x", Agents: []string{"w1"}}}}}
	if _, _, ok := b.fileTarget("ſ@x", "w1"); ok {
		t.Fatal("operator matched through a Unicode fold")
	}
	if _, _, ok := b.fileTarget("kſ@x", "w1"); ok {
		t.Fatal("contact matched through a Unicode fold")
	}
	if canon, _, ok := b.fileTarget("KS@X", "w1"); !ok || canon != "ks@x" {
		t.Fatalf("%q %v", canon, ok)
	}
}

// Past the hourly count the share is refused before the file is hashed:
// a missing file answers rate, not not-found.
func TestFilesShareRateIsCheckedBeforeTheHash(t *testing.T) {
	f := newFilesFixture(t)
	ck := chatfiles.Key("client@example.org")
	for i := 0; i < 20; i++ {
		f.put(t, "workers/scratch", fmt.Sprintf(".lever-files/out/%s/f%d.pdf", ck, i), "x")
		if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", fmt.Sprintf("f%d.pdf", i))); !res.OK {
			t.Fatalf("%d: %+v", i, res)
		}
	}
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "none.pdf")); res.Reason != "rate" {
		t.Fatalf("%+v", res)
	}
}

func TestFilesShareRefusesAHardLink(t *testing.T) {
	f := newFilesFixture(t)
	ck := chatfiles.Key("client@example.org")
	f.put(t, "workers/scratch", ".lever-files/in/"+ck+"/up.pdf", "x")
	f.put(t, "workers/scratch", ".lever-files/out/"+ck+"/keep.pdf", "x")
	ws := filepath.Join(f.tree, "workers/scratch/.lever-files")
	if err := os.Link(filepath.Join(ws, "in", ck, "up.pdf"), filepath.Join(ws, "out", ck, "up.pdf")); err != nil {
		t.Fatal(err)
	}
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "up.pdf")); res.Reason != "hard-link" {
		t.Fatalf("%+v", res)
	}
}

func TestShareFaultWords(t *testing.T) {
	for err, want := range map[error]string{
		fsutil.ErrSymlink: "symlink", fsutil.ErrHardLink: "hard-link", fsutil.ErrEscapesTree: "bad-path",
		fsutil.ErrNotRegularFile: "not-a-file", fsutil.ErrFileTooLarge: "too-large", os.ErrNotExist: "not-found",
	} {
		if got := shareFault(fmt.Errorf("x: %w", err)); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
	}
}

// Shares off: share_file answers shares-off and records nothing; the list
// still shows uploads and the shares already made.
func TestFilesSharesOff(t *testing.T) {
	f := newFilesFixture(t)
	ck := chatfiles.Key("client@example.org")
	f.put(t, "workers/scratch", ".lever-files/out/"+ck+"/a.pdf", "x")
	f.put(t, "workers/scratch", ".lever-files/out/"+ck+"/b.pdf", "y")
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "a.pdf")); !res.OK {
		t.Fatalf("%+v", res)
	}
	off := &filesFixture{verifyFixture: verifyBroker(t, nil, filesOpt(f.tree, f.dir), func(c *Config) { c.Files.NoShares = true }), tree: f.tree, dir: f.dir}
	if res := off.share(t, "scratch", "client@example.org", outPath("client@example.org", "b.pdf")); res.OK || res.Reason != "shares-off" {
		t.Fatalf("%+v", res)
	}
	if out := off.list(t, "scratch", "client@example.org"); !out.Enabled || len(out.Shares) != 1 {
		t.Fatalf("the share already made is still listed: %+v", out)
	}
}

func TestFilesExcludedLoginIsNoTarget(t *testing.T) {
	f := &filesFixture{tree: t.TempDir(), dir: t.TempDir()}
	f.verifyFixture = verifyBroker(t, nil, filesOpt(f.tree, f.dir), func(c *Config) {
		c.Files.Contacts = c.Files.Contacts[1:] // brokerctl drops a files: false login
	})
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/a.pdf", "x")
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "a.pdf")); res.Reason != "not-a-contact" {
		t.Fatalf("%+v", res)
	}
	if out := f.list(t, "scratch", "client@example.org"); out.Note != "not-a-contact" {
		t.Fatalf("%+v", out)
	}
}

// A share whose call times out (the route's TimeoutHandler answered 503)
// records nothing, so the agent's retry neither duplicates the share nor
// counts twice toward the hourly rate.
func TestFilesShareRecordsNothingAfterATimeout(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/a.pdf", "x")
	raw, _ := json.Marshal(wire.FileShareRequest{To: "client@example.org", Path: outPath("client@example.org", "a.pdf")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	afterShareHash = cancel // the deadline passes while the file is hashed
	t.Cleanup(func() { afterShareHash = nil })
	req := httptest.NewRequest("POST", wire.PathFilesShare, strings.NewReader(string(raw))).WithContext(ctx)
	req.TLS = fakeTLSWithCN("scratch")
	rec := httptest.NewRecorder()
	f.b.handleFilesShare(rec, req)
	var out wire.FileShareResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || out.Reason != "unavailable" {
		t.Fatalf("%+v", out)
	}
	if l := f.list(t, "scratch", "client@example.org"); len(l.Shares) != 0 {
		t.Fatalf("a timed-out share was recorded: %+v", l.Shares)
	}
	afterShareHash = nil
	if res := f.share(t, "scratch", "client@example.org", outPath("client@example.org", "a.pdf")); !res.OK {
		t.Fatalf("the retry: %+v", res)
	}
	if l := f.list(t, "scratch", "client@example.org"); len(l.Shares) != 1 {
		t.Fatalf("shares after the retry: %+v", l.Shares)
	}
}

// A share whose call times out while it waits for the ledger lock records
// nothing: the context is checked again once the lock is held.
func TestFilesShareRecordsNothingAfterATimeoutInTheLockWait(t *testing.T) {
	f := newFilesFixture(t)
	f.put(t, "workers/scratch", ".lever-files/out/"+chatfiles.Key("client@example.org")+"/a.pdf", "x")
	raw, _ := json.Marshal(wire.FileShareRequest{To: "client@example.org", Path: outPath("client@example.org", "a.pdf")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// After the hash another writer (the remote proxy) takes the ledger's
	// flock, so the share's record waits for it.
	var unlock func()
	hashed := make(chan struct{})
	afterShareHash = func() {
		var err error
		if unlock, err = hostledger.LockFile(filepath.Join(f.dir, "files-ledger", ".lock")); err != nil {
			panic(err)
		}
		close(hashed)
	}
	t.Cleanup(func() { afterShareHash = nil })
	req := httptest.NewRequest("POST", wire.PathFilesShare, strings.NewReader(string(raw))).WithContext(ctx)
	req.TLS = fakeTLSWithCN("scratch")
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.b.handleFilesShare(rec, req); close(done) }()
	<-hashed
	time.Sleep(50 * time.Millisecond) // the handler is past its context check, waiting for the lock
	cancel()
	unlock()
	<-done
	var out wire.FileShareResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || out.Reason != "unavailable" {
		t.Fatalf("%+v", out)
	}
	afterShareHash = nil
	if l := f.list(t, "scratch", "client@example.org"); len(l.Shares) != 0 {
		t.Fatalf("a share that timed out in the lock wait was recorded: %+v", l.Shares)
	}
}
