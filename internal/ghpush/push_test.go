package ghpush

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

type fixedToken string

func (f fixedToken) Token(context.Context, string) (string, error) { return string(f), nil }

const repo = "stevegeek/lever"

func newPusher(t *testing.T, r *testRemote) (*Pusher, string, *[][]string) {
	t.Helper()
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, BundleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin, _ := exec.LookPath("git")
	var argvs [][]string
	p := &Pusher{
		Tree: tree, State: state, Prefix: "agent/", BaseURL: r.srv.URL,
		Repos: map[string]bool{repo: true}, MaxBundle: 1 << 20, LockWait: time.Second,
		Tokens: fixedToken("ghs_TESTTOKEN"),
		Git:    Git{Bin: bin, Ceiling: state, Home: t.TempDir(), Timeout: time.Minute, AllowHTTP: true, Trace: func(a, _ []string) { argvs = append(argvs, a) }},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return p, tree, &argvs
}

// bundle builds a bundle in the agent tree from work.
func bundle(t *testing.T, work, tree, name string, refs ...string) {
	t.Helper()
	gitT(t, work, append([]string{"bundle", "create", filepath.Join(tree, BundleDir, name)}, refs...)...)
}

func commitOn(t *testing.T, work, branch, file string) {
	t.Helper()
	gitT(t, work, "checkout", "-B", branch)
	os.WriteFile(filepath.Join(work, file), []byte(file+"\n"), 0o644)
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-m", "change "+file)
}

func TestPushFullBundle(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, argvs := newPusher(t, r)
	commitOn(t, work, "agent/x", "b.txt")
	bundle(t, work, tree, "x.bundle", "agent/x")
	res, err := p.Push(context.Background(), "manager", repo, "agent/x", "x.bundle")
	if err != nil {
		t.Fatal(err)
	}
	if res.NewSHA != r.headSHA(t, repo, "agent/x") || res.OldSHA != "" {
		t.Fatalf("res %+v remote %s", res, r.headSHA(t, repo, "agent/x"))
	}
	if !strings.HasSuffix(res.CompareURL, "/stevegeek/lever/compare/main...agent/x") {
		t.Fatalf("compare url %q", res.CompareURL)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_TESTTOKEN"))
	if !slices.Contains(r.auths, want) {
		t.Fatalf("push did not authenticate: %q", r.auths)
	}
	for _, a := range *argvs {
		for _, s := range a {
			if strings.Contains(s, "ghs_TESTTOKEN") {
				t.Fatalf("token in argv %v", a)
			}
		}
		if slices.Contains(a, "protocol.file.allow=always") && !slices.Contains(a, "fetch") {
			t.Fatalf("file protocol on a non-fetch call: %v", a)
		}
	}
	out := gitT(t, filepath.Join(p.State, "mirrors", repo+".git"), "for-each-ref", "refs/tmp")
	if out != "" {
		t.Fatalf("temp ref left behind: %q", out)
	}
}

func TestPushIncrementalBundle(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	commitOn(t, work, "agent/inc", "c.txt")
	bundle(t, work, tree, "inc.bundle", "origin/main..agent/inc")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/inc", "inc.bundle"); err != nil {
		t.Fatal(err)
	}
}

func TestPushNonFastForward(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	commitOn(t, work, "agent/nf", "d.txt")
	bundle(t, work, tree, "nf1.bundle", "agent/nf")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/nf", "nf1.bundle"); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "checkout", "main")
	gitT(t, work, "branch", "-D", "agent/nf")
	commitOn(t, work, "agent/nf", "e.txt") // diverged history
	bundle(t, work, tree, "nf2.bundle", "agent/nf")
	_, err := p.Push(context.Background(), "manager", repo, "agent/nf", "nf2.bundle")
	if !errors.Is(err, ErrBranchMoved) {
		t.Fatalf("want ErrBranchMoved, got %v", err)
	}
}

func TestPushRefusals(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, argvs := newPusher(t, r)
	dir := filepath.Join(tree, BundleDir)
	commitOn(t, work, "agent/a", "f.txt")
	gitT(t, work, "tag", "v9.9.9")
	commitOn(t, work, "agent/b", "g.txt")
	bundle(t, work, tree, "tag.bundle", "agent/a", "v9.9.9")
	bundle(t, work, tree, "two.bundle", "agent/a", "agent/b")
	bundle(t, work, tree, "other.bundle", "agent/b")
	// Valid bundles for agent/a reached through a symlink and a hard link:
	// a missing link check would push them.
	bundle(t, work, tree, "a-src.bundle", "agent/a")
	bundle(t, work, tree, "h-src.bundle", "agent/a")
	if err := os.Symlink("a-src.bundle", filepath.Join(dir, "sym.bundle")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "h-src.bundle"), filepath.Join(dir, "hard.bundle")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bundle"), make([]byte, 2<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, repo, branch, bundle, want string
		is                               error
	}{
		{"tag in bundle", repo, "agent/a", "tag.bundle", "exactly one ref", nil},
		{"two heads", repo, "agent/a", "two.bundle", "exactly one ref", nil},
		{"wrong head", repo, "agent/a", "other.bundle", "refs/heads/agent/a", nil},
		{"too big", repo, "agent/a", "big.bundle", "too big", fsutil.ErrFileTooLarge},
		{"symlink", repo, "agent/a", "sym.bundle", "", fsutil.ErrSymlink},
		{"hard link", repo, "agent/a", "hard.bundle", "", fsutil.ErrHardLink},
		{"bad repo", "stevegeek/other", "agent/a", "a-src.bundle", "not one this tool", nil},
		{"bad branch", repo, "main", "a-src.bundle", "must start with", nil},
		{"bad bundle name", repo, "agent/a", "../x.bundle", "single file name", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(*argvs)
			remoteBefore := r.headSHA(t, repo, c.branch) // main pre-exists; others are empty
			_, err := p.Push(context.Background(), "manager", c.repo, c.branch, c.bundle)
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			if c.is != nil && !errors.Is(err, c.is) {
				t.Fatalf("want errors.Is %v, got %v", c.is, err)
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
			if strings.HasPrefix(c.name, "bad ") && len(*argvs) != before {
				t.Fatalf("name refusals must happen before any git call")
			}
			if r.headSHA(t, repo, c.branch) != remoteBefore {
				t.Fatalf("something was pushed to %s", c.branch)
			}
		})
	}
}

func TestPushMissingPrerequisites(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	commitOn(t, work, "agent/base", "h.txt") // never pushed: the remote lacks it
	commitOn(t, work, "agent/top", "i.txt")
	bundle(t, work, tree, "top.bundle", "agent/base..agent/top")
	_, err := p.Push(context.Background(), "manager", repo, "agent/top", "top.bundle")
	if err == nil || !strings.Contains(err.Error(), "prerequisites") {
		t.Fatalf("want a prerequisites refusal, got %v", err)
	}
}

func TestPushCorruptObjectRefusedByRemote(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	tree0 := gitT(t, work, "rev-parse", "HEAD^{tree}")
	parent := gitT(t, work, "rev-parse", "HEAD")
	// A commit with no committer line: fsck error missingCommitter.
	obj := "tree " + tree0 + "\nparent " + parent + "\nauthor t <t@t> 0 +0000\n\nbad\n"
	cmd := exec.Command("git", "-c", "credential.helper=", "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
	cmd.Dir, cmd.Stdin = work, strings.NewReader(obj)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	sha, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "update-ref", "refs/heads/agent/bad", strings.TrimSpace(string(sha)))
	bundle(t, work, tree, "bad.bundle", "agent/bad")
	// On git >= ~2.46 the mirror's own import refuses it (missingCommitter);
	// on older git the remote's receive.fsckObjects does. Either way: no push.
	if _, err := p.Push(context.Background(), "manager", repo, "agent/bad", "bad.bundle"); err == nil {
		t.Fatal("a corrupt object must not be pushed")
	}
	if r.headSHA(t, repo, "agent/bad") != "" {
		t.Fatal("remote received the corrupt branch")
	}
}

func TestPushLockBusy(t *testing.T) {
	r, _ := newTestRemote(t, repo)
	p, _, _ := newPusher(t, r)
	p.LockWait = 50 * time.Millisecond
	unlock, err := p.lock(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := p.lock(context.Background(), repo); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestPushReinitsHalfInitMirror(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	if err := os.MkdirAll(filepath.Join(p.State, "mirrors", repo+".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	commitOn(t, work, "agent/half", "h1.txt")
	bundle(t, work, tree, "half.bundle", "agent/half")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/half", "half.bundle"); err != nil {
		t.Fatal(err)
	}
	if r.headSHA(t, repo, "agent/half") == "" {
		t.Fatal("branch not pushed")
	}
}

func TestPushIgnoresRepoAboveState(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, argvs := newPusher(t, r)
	// A work tree above the state dir, with an origin that must never be used.
	above := t.TempDir()
	gitT(t, above, "init", "-b", "main")
	gitT(t, above, "remote", "add", "origin", "http://127.0.0.1:1/nope.git")
	state := filepath.Join(above, "state")
	if err := os.MkdirAll(filepath.Join(state, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	p.State = state
	p.Git.Ceiling = state
	// Half-init mirror: without --git-dir git would discover the repo above.
	if err := os.MkdirAll(filepath.Join(state, "mirrors", repo+".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	commitOn(t, work, "agent/above", "ab.txt")
	bundle(t, work, tree, "above.bundle", "agent/above")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/above", "above.bundle"); err != nil {
		t.Fatal(err)
	}
	if r.headSHA(t, repo, "agent/above") == "" {
		t.Fatal("branch not pushed to the test remote")
	}
	for _, a := range *argvs {
		if len(a) == 0 {
			continue
		}
		hasDir := false
		for _, s := range a {
			if strings.HasPrefix(s, "--git-dir=") {
				hasDir = true
			}
		}
		isInit := slices.Contains(a, "init")
		if !hasDir && !isInit {
			t.Fatalf("git call without --git-dir: %v", a)
		}
	}
	if _, err := os.Stat(filepath.Join(above, ".git", "refs", "tmp")); err == nil {
		t.Fatal("the repo above the state dir was touched")
	}
}
func TestPushRefusesAnnotatedTagHead(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	commitOn(t, work, "agent/tagh", "t1.txt")
	gitT(t, work, "tag", "-a", "-m", "annotated", "vt1")
	tagObj := gitT(t, work, "rev-parse", "vt1")
	// update-ref refuses a non-commit under refs/heads; write the ref file.
	refFile := filepath.Join(work, ".git", "refs", "heads", "agent", "tagobj")
	if err := os.WriteFile(refFile, []byte(tagObj+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle(t, work, tree, "tagobj.bundle", "agent/tagobj")
	_, err := p.Push(context.Background(), "manager", repo, "agent/tagobj", "tagobj.bundle")
	if err == nil || !strings.Contains(err.Error(), "not a commit") {
		t.Fatalf("want a not-a-commit refusal, got %v", err)
	}
	if r.headSHA(t, repo, "agent/tagobj") != "" {
		t.Fatal("tag object was pushed")
	}
}
func TestPushBranchMovedKeepsCause(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	commitOn(t, work, "agent/mc", "m1.txt")
	bundle(t, work, tree, "mc1.bundle", "agent/mc")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/mc", "mc1.bundle"); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "checkout", "main")
	gitT(t, work, "branch", "-D", "agent/mc")
	commitOn(t, work, "agent/mc", "m2.txt")
	bundle(t, work, tree, "mc2.bundle", "agent/mc")
	_, err := p.Push(context.Background(), "manager", repo, "agent/mc", "mc2.bundle")
	if !errors.Is(err, ErrBranchMoved) || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("want ErrBranchMoved with the git cause, got %v", err)
	}
}

func TestPushOldSHAExactRefMatch(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	commitOn(t, work, "agent/refs/heads/agent/x", "d1.txt")
	bundle(t, work, tree, "decoy.bundle", "agent/refs/heads/agent/x")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/refs/heads/agent/x", "decoy.bundle"); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "checkout", "main")
	commitOn(t, work, "agent/x", "x1.txt")
	bundle(t, work, tree, "x1.bundle", "agent/x")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/x", "x1.bundle"); err != nil {
		t.Fatal(err)
	}
	first := r.headSHA(t, repo, "agent/x")
	commitOn(t, work, "agent/x", "x2.txt")
	bundle(t, work, tree, "x2.bundle", "agent/x")
	res, err := p.Push(context.Background(), "manager", repo, "agent/x", "x2.bundle")
	if err != nil {
		t.Fatal(err)
	}
	if res.OldSHA != first {
		t.Fatalf("OldSHA %s, want %s", res.OldSHA, first)
	}
}

func dirSize(t *testing.T, d string) (n int64) {
	t.Helper()
	filepath.Walk(d, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// tagHeadBundle writes a bundle whose one head is an annotated tag over a new
// commit with a random blob: it passes verify and the import, then is refused.
func tagHeadBundle(t *testing.T, work, tree, name string, size int) {
	t.Helper()
	gitT(t, work, "checkout", "-q", "-B", "agent/j", "origin/main")
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "junk.bin"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-q", "-m", "junk "+name)
	gitT(t, work, "tag", "-f", "-a", "-m", "x", "t-"+name)
	tagObj := gitT(t, work, "rev-parse", "t-"+name)
	if err := os.WriteFile(filepath.Join(work, ".git", "refs", "heads", "agent", "tagobj"), []byte(tagObj+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle(t, work, tree, name, "agent/tagobj", "^origin/main")
}

func TestPushRefusedImportsLeaveMirrorUnchanged(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	p.MaxBundle = 4 << 20
	mirror := filepath.Join(p.State, "mirrors", repo+".git")
	var before int64
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("j%d.bundle", i)
		tagHeadBundle(t, work, tree, name, 1<<20)
		if _, err := p.Push(context.Background(), "manager", repo, "agent/tagobj", name); err == nil || !strings.Contains(err.Error(), "not a commit") {
			t.Fatalf("push %d: want a not-a-commit refusal, got %v", i, err)
		}
		if i == 0 {
			before = dirSize(t, filepath.Join(mirror, "objects"))
			continue
		}
		if got := dirSize(t, filepath.Join(mirror, "objects")); got != before {
			t.Fatalf("push %d: mirror objects grew from %d to %d bytes", i, before, got)
		}
	}
	if before > 1<<20 {
		t.Fatalf("the refused bundle reached the mirror: %d bytes", before)
	}
	if ents, _ := os.ReadDir(filepath.Join(p.State, "tmp")); len(ents) != 0 {
		t.Fatalf("temp dirs left behind: %v", ents)
	}
}

func TestPushFailedFsckLeavesNoTempPack(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	tree0 := gitT(t, work, "rev-parse", "HEAD^{tree}")
	parent := gitT(t, work, "rev-parse", "HEAD")
	obj := "tree " + tree0 + "\nparent " + parent + "\nauthor t <t@t> 0 +0000\n\nbad\n"
	cmd := exec.Command("git", "-c", "credential.helper=", "hash-object", "-t", "commit", "--literally", "-w", "--stdin")
	cmd.Dir, cmd.Stdin = work, strings.NewReader(obj)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	sha, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "update-ref", "refs/heads/agent/bad", strings.TrimSpace(string(sha)))
	bundle(t, work, tree, "bad.bundle", "agent/bad")
	if _, err := p.Push(context.Background(), "manager", repo, "agent/bad", "bad.bundle"); err == nil || !strings.Contains(err.Error(), "import bundle") {
		t.Fatalf("want an import (fsck) refusal, got %v", err)
	}
	filepath.Walk(p.State, func(path string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(fi.Name(), "tmp_pack_") {
			t.Errorf("temp pack left behind: %s", path)
		}
		return nil
	})
}

func TestPushRefusedObjectsAreNoPrerequisiteLater(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	// Caller A: a commit that is imported, then refused (tag head).
	tagHeadBundle(t, work, tree, "a.bundle", 1024)
	if _, err := p.Push(context.Background(), "worker-a", repo, "agent/tagobj", "a.bundle"); err == nil {
		t.Fatal("want a refusal")
	}
	// Caller B: a bundle that needs A's refused commit as a prerequisite.
	gitT(t, work, "checkout", "-q", "-B", "agent/b", "agent/j")
	commitOn(t, work, "agent/b", "b2.txt")
	bundle(t, work, tree, "b.bundle", "agent/j..agent/b")
	_, err := p.Push(context.Background(), "worker-b", repo, "agent/b", "b.bundle")
	if err == nil || !strings.Contains(err.Error(), "prerequisites") {
		t.Fatalf("want a prerequisites refusal, got %v", err)
	}
	if r.headSHA(t, repo, "agent/b") != "" {
		t.Fatal("caller B pushed on top of caller A's refused objects")
	}
}

func TestPushImportBudget(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	commitOn(t, work, "agent/q", "q.txt")
	bundle(t, work, tree, "q.bundle", "origin/main..agent/q")
	fi, err := os.Stat(filepath.Join(tree, BundleDir, "q.bundle"))
	if err != nil {
		t.Fatal(err)
	}
	p.ImportBudget = fi.Size()*2 + 1
	for i := 0; i < 2; i++ {
		if _, err := p.Push(context.Background(), "manager", repo, "agent/q", "q.bundle"); err != nil {
			t.Fatalf("push %d within the budget: %v", i, err)
		}
	}
	if _, err := p.Push(context.Background(), "manager", repo, "agent/q", "q.bundle"); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("want a budget refusal, got %v", err)
	}
	// The budget is per caller.
	if _, err := p.Push(context.Background(), "worker", repo, "agent/q", "q.bundle"); err != nil {
		t.Fatalf("another caller has its own budget: %v", err)
	}
	// Spend older than the window does not count.
	p.now = func() time.Time { return time.Now().Add(importWindow + time.Minute) }
	if _, err := p.Push(context.Background(), "manager", repo, "agent/q", "q.bundle"); err != nil {
		t.Fatalf("budget did not refill after the window: %v", err)
	}
}

func TestPushErrorsHideHostPaths(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	// fsutil names the tree path for a directory in place of a bundle.
	if err := os.Mkdir(filepath.Join(tree, BundleDir, "d.bundle"), 0o755); err != nil {
		t.Fatal(err)
	}
	// git names the state path when it cannot create the mirror.
	owner := filepath.Join(p.State, "mirrors", "stevegeek")
	if err := os.MkdirAll(owner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(owner, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(owner, 0o700) })
	commitOn(t, work, "agent/x", "q.txt")
	bundle(t, work, tree, "q.bundle", "agent/x")
	for _, c := range []struct{ bundle, want string }{{"d.bundle", "<tree>"}, {"q.bundle", "<state>"}} {
		_, err := p.Push(context.Background(), "manager", repo, "agent/x", c.bundle)
		if err == nil {
			t.Fatalf("%s: want a refusal", c.bundle)
		}
		msg := err.Error()
		for _, raw := range []string{tree, p.State, os.TempDir()} {
			if real, e := filepath.EvalSymlinks(raw); e == nil && strings.Contains(msg, real) || strings.Contains(msg, raw) {
				t.Fatalf("%s: host path %s in %q", c.bundle, raw, msg)
			}
		}
		if !strings.Contains(msg, c.want) {
			t.Fatalf("%s: want placeholder %s in %q", c.bundle, c.want, msg)
		}
	}
}

func TestHidePathsKeepsErrorsIs(t *testing.T) {
	p := &Pusher{Tree: "/home/u/tree", State: "/home/u/state", Git: Git{Home: "/home/u/state/home"}}
	err := p.hidePaths(fmt.Errorf("x /home/u/state/tmp/push-123/work.git and /home/u/tree/a: %w", fsutil.ErrSymlink))
	if got := err.Error(); got != "x <tmp>/work.git and <tree>/a: "+fsutil.ErrSymlink.Error() {
		t.Fatalf("got %q", got)
	}
	if !errors.Is(err, fsutil.ErrSymlink) {
		t.Fatal("errors.Is lost")
	}
}

type blockingToken struct{}

func (blockingToken) Token(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestPushOverallDeadline(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, tree, _ := newPusher(t, r)
	p.Tokens = blockingToken{}
	p.Deadline = 2 * time.Second
	commitOn(t, work, "agent/dl", "dl.txt")
	bundle(t, work, tree, "dl.bundle", "agent/dl")
	start := time.Now()
	_, err := p.Push(context.Background(), "manager", repo, "agent/dl", "dl.bundle")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("push took %v past a %v deadline", d, p.Deadline)
	}
	// The lock is free again.
	unlock, err := p.lock(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
