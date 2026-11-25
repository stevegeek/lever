package ghpush

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A repo whose default branch is master (T0650): push and fetch must not
// ask GitHub for refs/heads/main.
func TestPushAndFetchMasterDefault(t *testing.T) {
	r, work := newTestRemoteBranch(t, repo, "master")
	r.private = true
	p, tree, _ := newPusher(t, r)
	p.Scoped = fixedToken("ghs_TESTTOKEN")

	commitOn(t, work, "agent/m", "m.txt")
	// Incremental: the prerequisite is master's head, which only the mirror
	// refresh of the default branch provides.
	bundle(t, work, tree, "m.bundle", "origin/master..agent/m")
	res, err := p.Push(context.Background(), "manager", repo, "agent/m", "m.bundle")
	if err != nil {
		t.Fatal(err)
	}
	if res.NewSHA != r.headSHA(t, repo, "agent/m") {
		t.Fatalf("res %+v remote %s", res, r.headSHA(t, repo, "agent/m"))
	}
	if !strings.HasSuffix(res.CompareURL, "/stevegeek/lever/compare/master...agent/m") {
		t.Fatalf("compare url %q", res.CompareURL)
	}

	for _, br := range []string{"master", "agent/m"} {
		fr, err := p.Fetch(context.Background(), "manager", repo, br, "f.bundle")
		if err != nil {
			t.Fatalf("fetch %s: %v", br, err)
		}
		if fr.SHA != r.headSHA(t, repo, br) {
			t.Fatalf("%s: sha %s want %s", br, fr.SHA, r.headSHA(t, repo, br))
		}
	}
	out := gitT(t, filepath.Join(p.State, "mirrors", repo+".git"), "for-each-ref", "--format=%(refname)")
	if out != mirrorRef {
		t.Fatalf("mirror should hold only the default branch: %q", out)
	}
}

// An empty repository has no default branch to refresh: a clear error, not
// git's "couldn't find remote ref".
func TestRefreshEmptyRepo(t *testing.T) {
	r, _ := newTestRemote(t, repo)
	p, _, _ := newPusher(t, r)
	p.Scoped = fixedToken("ghs_TESTTOKEN")
	bare := filepath.Join(r.root, repo+".git")
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	gitT(t, r.root, "init", "--bare", "-b", "main", bare)
	_, err := p.Fetch(context.Background(), "manager", repo, "main", "e.bundle")
	if err == nil || !strings.Contains(err.Error(), "no default branch") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDefaultBranch(t *testing.T) {
	sha := strings.Repeat("a", 40)
	head := sha + "\tHEAD\n"
	for _, c := range []struct{ out, want, err string }{
		{"ref: refs/heads/main\tHEAD\n" + sha + "\tHEAD\n", "main", ""},
		{"ref: refs/heads/master\tHEAD\n" + sha + "\tHEAD\n", "master", ""},
		{"ref: refs/heads/release/v2\tHEAD\n" + sha + "\tHEAD\n", "release/v2", ""},
		{"", "", "no default branch"},
		{sha + "\tHEAD\n", "", "no default branch"}, // detached HEAD: no symref
		{"ref: refs/tags/v1\tHEAD\n" + head, "", "not a branch"},
		// An unborn HEAD (empty repo, if the remote reports its symref).
		{"ref: refs/heads/main\tHEAD\n", "", "no default branch"},
		{"ref: refs/heads/main\tHEAD\nnot-a-sha\tHEAD\n", "", "no default branch"},
		// The name comes from the remote: nothing that could bend a refspec.
		{"ref: refs/heads/a:b\tHEAD\n" + head, "", "only letters"},
		{"ref: refs/heads/x..y\tHEAD\n" + head, "", "'..'"},
		{"ref: refs/heads/-x\tHEAD\n" + head, "", "invalid path component"},
		{"ref: refs/heads/HEAD\tHEAD\n" + head, "", "name a branch"},
		{"ref: refs/heads/a b\tHEAD\n" + head, "", "only letters"},
	} {
		got, err := parseDefaultBranch(c.out)
		if c.err == "" {
			if err != nil || got != c.want {
				t.Errorf("%q: got %q, %v; want %q", c.out, got, err, c.want)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("%q: err = %v, want %q", c.out, err, c.err)
		}
	}
}

// The default branch is renamed between calls, including to a name that
// would clash as a ref path with the old one (release -> release/v2): the
// mirror keeps one fixed ref, so every refresh still works.
func TestDefaultBranchRenamed(t *testing.T) {
	r, work := newTestRemoteBranch(t, repo, "release")
	p, tree, _ := newPusher(t, r)
	p.Scoped = fixedToken("ghs_TESTTOKEN")
	bare := filepath.Join(r.root, repo+".git")
	mirror := filepath.Join(p.State, "mirrors", repo+".git")
	for i, def := range []string{"release", "release/v2", "main"} {
		if i > 0 {
			prev := []string{"release", "release/v2", "main"}[i-1]
			gitT(t, bare, "branch", "-m", prev, def)
		}
		fr, err := p.Fetch(context.Background(), "manager", repo, def, "r.bundle")
		if err != nil {
			t.Fatalf("default %s: %v", def, err)
		}
		if fr.SHA != r.headSHA(t, repo, def) {
			t.Fatalf("default %s: sha %s", def, fr.SHA)
		}
		if out := gitT(t, mirror, "for-each-ref", "--format=%(refname)"); out != mirrorRef {
			t.Fatalf("default %s: mirror refs %q", def, out)
		}
	}
	commitOn(t, work, "agent/after-rename", "ar.txt")
	bundle(t, work, tree, "ar.bundle", "agent/after-rename")
	res, err := p.Push(context.Background(), "manager", repo, "agent/after-rename", "ar.bundle")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.CompareURL, "/compare/main...agent/after-rename") {
		t.Fatalf("compare url %q", res.CompareURL)
	}
}

// A default branch that happens to carry the push prefix is still never a
// push target.
func TestPushRefusesDefaultBranch(t *testing.T) {
	r, work := newTestRemoteBranch(t, repo, "agent/trunk")
	p, tree, _ := newPusher(t, r)
	before := r.headSHA(t, repo, "agent/trunk")
	commitOn(t, work, "agent/trunk", "t.txt")
	bundle(t, work, tree, "t.bundle", "agent/trunk")
	_, err := p.Push(context.Background(), "manager", repo, "agent/trunk", "t.bundle")
	if err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("err = %v", err)
	}
	if got := r.headSHA(t, repo, "agent/trunk"); got != before {
		t.Fatalf("default branch moved: %s -> %s", before, got)
	}
}
