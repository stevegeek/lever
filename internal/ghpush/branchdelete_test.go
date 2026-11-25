package ghpush

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
)

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
	res, err := o.BranchDelete(context.Background(), "manager", repo, "agent/x")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.SHA != shaHead || res.MergedBy != "ancestor" || res.Branch != "agent/x" {
		t.Fatalf("res %+v", res)
	}
	if !slices.Equal(f.deleted, []string{"agent/x"}) {
		t.Fatalf("deleted %v", f.deleted)
	}
	if len(tk.calls) != 1 || !maps.Equal(tk.calls[0].perms, Permissions{"contents": "write", "pull_requests": "read"}) {
		t.Fatalf("token calls %v", tk.calls)
	}
	if log := logBuf.String(); !strings.Contains(log, "github.branch_delete") || !strings.Contains(log, "decision=allow") ||
		!strings.Contains(log, "merged_by=ancestor") {
		t.Fatalf("audit line %s", log)
	}
}

func TestBranchDeleteIdentical(t *testing.T) {
	f := newFakeGH(t)
	f.compare = map[string]string{shaHead: "identical"}
	o, _, _ := newOps(t, f)
	if _, err := o.BranchDelete(context.Background(), "manager", repo, "agent/x"); err != nil {
		t.Fatal(err)
	}
}

// A squash merge leaves the head outside the default branch; the merged PR
// at that exact head is the proof.
func TestBranchDeleteSquashMergedPR(t *testing.T) {
	f := newFakeGH(t)
	f.addPull(7, mergedPull(shaHead))
	o, _, _ := newOps(t, f)
	res, err := o.BranchDelete(context.Background(), "manager", repo, "agent/x")
	if err != nil {
		t.Fatal(err)
	}
	if res.MergedBy != "pr #7" || !slices.Equal(f.deleted, []string{"agent/x"}) {
		t.Fatalf("res %+v, deleted %v", res, f.deleted)
	}
}

func TestBranchDeleteRefusals(t *testing.T) {
	cases := map[string]struct {
		branch string
		setup  func(f *fakeGH)
		want   string
	}{
		"not the prefix": {"feature/x", nil, `must start with "agent/"`},
		"protected name": {"agent/main", nil, "protected"},
		"missing":        {"agent/gone", nil, "does not exist"},
		"default branch": {"agent/x", func(f *fakeGH) { f.defaultBranch = "agent/x" }, "is the default branch"},
		"diverged":       {"agent/x", nil, "has commits that are not in main"},
		"ahead":          {"agent/x", func(f *fakeGH) { f.compare = map[string]string{shaHead: "ahead"} }, "has commits that are not in main"},
		"open PR":        {"agent/x", func(f *fakeGH) { f.addPull(7, nil); f.compare = map[string]string{shaHead: "behind"} }, "open PR #7"},
		"compare fails":  {"agent/x", func(f *fakeGH) { f.compareStatus = 500 }, "could not be checked"},
		"moved after PR": {"agent/x", func(f *fakeGH) { f.addPull(7, mergedPull(shaMain)) }, "has commits that are not in main"},
		"PR not merged":  {"agent/x", func(f *fakeGH) { f.addPull(7, func(p map[string]any) { p["state"] = "closed" }) }, "has commits"},
		"PR into another": {"agent/x", func(f *fakeGH) {
			f.addPull(7, func(p map[string]any) { mergedPull(shaHead)(p); p["base"].(map[string]any)["ref"] = "dev" })
		}, "has commits"},
		"PR from a fork": {"agent/x", func(f *fakeGH) {
			f.addPull(7, func(p map[string]any) {
				mergedPull(shaHead)(p)
				p["head"].(map[string]any)["repo"] = map[string]any{"full_name": "someone/lever"}
			})
		}, "has commits"},
		"moved meanwhile": {"agent/x", func(f *fakeGH) {
			f.compare = map[string]string{shaHead: "behind"}
			f.moveOnRead = map[string]string{"agent/x": shaNew}
		}, "moved from"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeGH(t)
			if c.setup != nil {
				c.setup(f)
			}
			o, _, logBuf := newOps(t, f)
			_, err := o.BranchDelete(context.Background(), "manager", repo, c.branch)
			wantErr(t, err, c.want)
			if len(f.deleted) != 0 {
				t.Fatalf("deleted %v", f.deleted)
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
	_, err := o.BranchDelete(context.Background(), "manager", "someone/else", "agent/x")
	wantErr(t, err, "someone/else")
	if len(tk.calls) != 0 || len(f.reqs) != 0 {
		t.Fatalf("token calls %v, requests %v", tk.calls, f.reqs)
	}
}
