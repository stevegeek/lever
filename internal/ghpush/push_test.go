package ghpush

import (
	"context"
	"encoding/base64"
	"errors"
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
		Git:    Git{Bin: bin, Home: t.TempDir(), Timeout: time.Minute, AllowHTTP: true, Trace: func(a, _ []string) { argvs = append(argvs, a) }},
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
