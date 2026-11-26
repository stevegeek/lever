package ghpush

import (
	"context"
	"encoding/base64"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRefs is a RefDeleter on fakeGH: it deletes only at the expected head.
type fakeRefs struct {
	f       *fakeGH
	deleted []string
	tokens  []string
	err     error
}

func (r *fakeRefs) DeleteBranch(_ context.Context, repo, branch, sha, token string) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.tokens = append(r.tokens, token)
	if r.err != nil {
		return r.err
	}
	if r.f.heads[branch] != sha {
		return errors.New("branch moved on GitHub since it was checked; nothing deleted")
	}
	delete(r.f.heads, branch)
	r.deleted = append(r.deleted, branch)
	return nil
}

func newDeleteOps(t *testing.T, f *fakeGH) (*Ops, *fakeRefs) {
	t.Helper()
	o, _, _ := newOps(t, f)
	refs := &fakeRefs{f: f}
	o.Refs = refs
	return o, refs
}

// mergedPull is a closed PR from agent/x into main, merged at head.
func mergedPull(head string) func(p map[string]any) {
	return func(p map[string]any) {
		p["state"], p["merged"], p["merged_at"] = "closed", true, "2026-11-25T10:00:00Z"
		p["head"].(map[string]any)["sha"] = head
	}
}

func TestBranchDeleteAncestor(t *testing.T) {
	f := newFakeGH(t)
	f.compare = map[string]string{shaHead: "behind"}
	o, tk, logBuf := newOps(t, f)
	refs := &fakeRefs{f: f}
	o.Refs = refs
	res, err := o.BranchDelete(context.Background(), "manager", repo, "agent/x")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.SHA != shaHead || res.MergedBy != "ancestor" || res.Branch != "agent/x" {
		t.Fatalf("res %+v", res)
	}
	if !slices.Equal(refs.deleted, []string{"agent/x"}) || !slices.Equal(refs.tokens, []string{scoped}) {
		t.Fatalf("deleted %v with %v", refs.deleted, refs.tokens)
	}
	if len(tk.calls) != 1 || !maps.Equal(tk.calls[0].perms, Permissions{"contents": "write", "pull_requests": "read"}) {
		t.Fatalf("token calls %v", tk.calls)
	}
	if log := logBuf.String(); !strings.Contains(log, "github.branch_delete") || !strings.Contains(log, "decision=allow") ||
		!strings.Contains(log, "merged_by=ancestor") {
		t.Fatalf("audit line %s", log)
	}
	if !slices.ContainsFunc(f.reqs, func(r string) bool { return strings.HasPrefix(r, "GET /repos/"+repo+"/compare/heads/main...") }) {
		t.Fatalf("no compare against heads/main: %v", f.reqs)
	}
}

func TestBranchDeleteIdentical(t *testing.T) {
	f := newFakeGH(t)
	f.compare = map[string]string{shaHead: "identical"}
	o, refs := newDeleteOps(t, f)
	if _, err := o.BranchDelete(context.Background(), "manager", repo, "agent/x"); err != nil {
		t.Fatal(err)
	}
	if len(refs.deleted) != 1 {
		t.Fatalf("deleted %v", refs.deleted)
	}
}

// A squash merge leaves the head outside the default branch; the merged PR
// at that exact head is the proof.
func TestBranchDeleteSquashMergedPR(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, mergedPull(shaHead))
	o, refs := newDeleteOps(t, f)
	res, err := o.BranchDelete(context.Background(), "manager", repo, "agent/x")
	if err != nil {
		t.Fatal(err)
	}
	if res.MergedBy != "pr #7" || !slices.Equal(refs.deleted, []string{"agent/x"}) {
		t.Fatalf("res %+v, deleted %v", res, refs.deleted)
	}
}

func TestBranchDeleteRefusals(t *testing.T) {
	behind := func(f *fakeGH) { f.compare = map[string]string{shaHead: "behind"} }
	cases := map[string]struct {
		branch string
		setup  func(f *fakeGH, r *fakeRefs)
		want   string
	}{
		"not the prefix": {"feature/x", nil, `must start with "agent/"`},
		"protected name": {"agent/main", nil, "protected"},
		"missing":        {"agent/gone", nil, "does not exist"},
		"default branch": {"agent/x", func(f *fakeGH, _ *fakeRefs) { f.defaultBranch = "agent/x" }, "is the default branch"},
		"diverged":       {"agent/x", nil, "has commits that are not in main"},
		"ahead":          {"agent/x", func(f *fakeGH, _ *fakeRefs) { f.compare = map[string]string{shaHead: "ahead"} }, "has commits that are not in main"},
		"open PR":        {"agent/x", func(f *fakeGH, _ *fakeRefs) { f.addPull(7, nil); behind(f) }, "open PR #7"},
		"base of an open PR": {"agent/x", func(f *fakeGH, _ *fakeRefs) {
			behind(f)
			f.addPull(8, func(p map[string]any) {
				p["head"].(map[string]any)["ref"] = "agent/y"
				p["base"].(map[string]any)["ref"] = "agent/x"
			})
		}, "base of open PR #8"},
		"compare fails":  {"agent/x", func(f *fakeGH, _ *fakeRefs) { f.compareStatus = 500 }, "could not be checked"},
		"PR list fails":  {"agent/x", func(f *fakeGH, _ *fakeRefs) { behind(f); f.pullsStatus = 500 }, "list"},
		"moved after PR": {"agent/x", func(f *fakeGH, _ *fakeRefs) { f.addPull(7, mergedPull(shaMain)) }, "has commits that are not in main"},
		"PR not merged":  {"agent/x", func(f *fakeGH, _ *fakeRefs) { f.addPull(7, func(p map[string]any) { p["state"] = "closed" }) }, "has commits"},
		"PR merged_at empty": {"agent/x", func(f *fakeGH, _ *fakeRefs) {
			f.addPull(7, func(p map[string]any) { mergedPull(shaHead)(p); p["merged_at"] = "" })
		}, "has commits"},
		"PR into another": {"agent/x", func(f *fakeGH, _ *fakeRefs) {
			f.addPull(7, func(p map[string]any) { mergedPull(shaHead)(p); p["base"].(map[string]any)["ref"] = "dev" })
		}, "has commits"},
		"PR head ref differs": {"agent/x", func(f *fakeGH, _ *fakeRefs) {
			// GitHub answered with a PR for another head ref.
			f.addPull(7, func(p map[string]any) { mergedPull(shaHead)(p); p["head"].(map[string]any)["ref"] = "agent/x2" })
			f.closedIgnoresHead = true
		}, "has commits"},
		"PR from a fork": {"agent/x", func(f *fakeGH, _ *fakeRefs) {
			f.addPull(7, func(p map[string]any) {
				mergedPull(shaHead)(p)
				p["head"].(map[string]any)["repo"] = map[string]any{"full_name": "someone/lever"}
			})
		}, "has commits"},
		"moved meanwhile": {"agent/x", func(f *fakeGH, r *fakeRefs) {
			behind(f)
			r.err = errors.New(`branch "agent/x" moved on GitHub since it was checked; nothing deleted`)
		}, "moved on GitHub"},
		"delete refused": {"agent/x", func(f *fakeGH, r *fakeRefs) {
			behind(f)
			r.err = errors.New("delete branch agent/x: push declined due to repository rule violations")
		}, "rule violations"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGH(t)
			o, _, logBuf := newOps(t, f)
			refs := &fakeRefs{f: f}
			o.Refs = refs
			if c.setup != nil {
				c.setup(f, refs)
			}
			_, err := o.BranchDelete(context.Background(), "manager", repo, c.branch)
			wantErr(t, err, c.want)
			if len(refs.deleted) != 0 {
				t.Fatalf("deleted %v", refs.deleted)
			}
			if !strings.Contains(logBuf.String(), "decision=deny") {
				t.Fatalf("audit line %s", logBuf.String())
			}
		})
	}
}

func TestBranchDeleteRepoNotAllowed(t *testing.T) {
	f := newFakeGH(t)
	o, tk, _ := newOps(t, f)
	o.Refs = &fakeRefs{f: f}
	_, err := o.BranchDelete(context.Background(), "manager", "someone/else", "agent/x")
	wantErr(t, err, "someone/else")
	if len(tk.calls) != 0 || len(f.reqs) != 0 {
		t.Fatalf("token calls %v, requests %v", tk.calls, f.reqs)
	}
}

func TestBranchDeleteNotConfigured(t *testing.T) {
	f := newFakeGH(t)
	f.compare = map[string]string{shaHead: "behind"}
	o, _, _ := newOps(t, f)
	_, err := o.BranchDelete(context.Background(), "manager", repo, "agent/x")
	wantErr(t, err, "not configured")
}

// The lease against a real git remote: the delete happens only at the
// expected head, authenticated by header, never with the token in argv.
func TestPusherDeleteBranchLease(t *testing.T) {
	r, work := newTestRemote(t, repo)
	p, _, argvs := newPusher(t, r)
	commitOn(t, work, "agent/x", "a.txt")
	gitT(t, work, "push", r.srv.URL+"/"+repo, "agent/x")
	head := gitT(t, work, "rev-parse", "agent/x")
	r.mu.Lock()
	r.private, r.auths = true, nil
	r.mu.Unlock()
	ctx := context.Background()

	stale := strings.Repeat("1", 40)
	err := p.DeleteBranch(ctx, repo, "agent/x", stale, "ghs_TESTTOKEN")
	if err == nil || !strings.Contains(err.Error(), "moved on GitHub") {
		t.Fatalf("stale lease: %v", err)
	}
	if r.headSHA(t, repo, "agent/x") != head {
		t.Fatal("branch gone after a stale lease")
	}

	if err := p.DeleteBranch(ctx, repo, "agent/x", head, "ghs_TESTTOKEN"); err != nil {
		t.Fatal(err)
	}
	if got := r.headSHA(t, repo, "agent/x"); got != "" {
		t.Fatalf("branch still there: %q", got)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_TESTTOKEN"))
	auths := r.seenAuths()
	if !slices.Contains(auths, want) || slices.Contains(auths, "") {
		t.Fatalf("delete did not authenticate on every request: %q", auths)
	}
	for _, a := range *argvs {
		if strings.Contains(strings.Join(a, " "), "ghs_TESTTOKEN") {
			t.Fatalf("token in argv: %v", a)
		}
	}

	// Already gone: refused, not reported as deleted.
	if err := p.DeleteBranch(ctx, repo, "agent/x", head, "ghs_TESTTOKEN"); err == nil {
		t.Fatal("deleting a missing branch succeeded")
	}

	for _, c := range []struct{ repo, branch, sha, want string }{
		{"someone/else", "agent/x", head, "someone/else"},
		{repo, "main", head, `must start with "agent/"`},
		{repo, "agent/x", "HEAD", "unexpected head"},
	} {
		if err := p.DeleteBranch(ctx, c.repo, c.branch, c.sha, "t"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
}

// A push holding the repo's lock makes a delete wait, then give up busy.
func TestPusherDeleteBranchWaitsForPush(t *testing.T) {
	r, _ := newTestRemote(t, repo)
	p, _, _ := newPusher(t, r)
	p.LockWait = 50 * time.Millisecond
	unlock, err := p.lock(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := p.DeleteBranch(context.Background(), repo, "agent/x", strings.Repeat("1", 40), "t"); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}
