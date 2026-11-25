package ghpush

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// OpBranchDelete deletes a merged branch.
const OpBranchDelete = "branch_delete"

// maxClosedPRs is how many closed PRs with the branch as head are read when
// looking for the one that merged it.
const maxClosedPRs = 30

// BranchDeleteResult is what branch_delete returns. MergedBy is how the head
// was found to be merged: "ancestor" when the default branch contains it, or
// "pr #N" for a PR into the default branch merged with that exact head (a
// squash or rebase merge).
type BranchDeleteResult struct {
	OK       bool   `json:"ok"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	SHA      string `json:"deleted_sha"`
	MergedBy string `json:"merged_by"`
}

// RefDeleter deletes a branch only if it still points at sha.
type RefDeleter interface {
	DeleteBranch(ctx context.Context, repo, branch, sha, token string) error
}

type ghClosedPull struct {
	Number   int        `json:"number"`
	MergedAt *string    `json:"merged_at"`
	Head     ghPullSide `json:"head"`
	Base     ghPullSide `json:"base"`
}

// BranchDelete deletes branch when everything on it is in the default
// branch, so a delete never loses work. It refuses a branch with an open PR
// from it or into it (GitHub closes a PR whose base is deleted). The delete
// is conditional on the head that was checked (RefDeleter).
func (o *Ops) BranchDelete(ctx context.Context, caller, repo, branch string) (res BranchDeleteResult, err error) {
	var sha, mergedBy string
	defer func() {
		o.audit(OpBranchDelete, err, "caller", caller, "repo", repo, "branch", branch, "sha", sha, "merged_by", mergedBy)
	}()
	if err := ValidateRepo(repo, o.Repos); err != nil {
		return res, err
	}
	if err := ValidateBranch(branch, o.Prefix); err != nil {
		return res, err
	}
	ctx, cancel, tok, err := o.begin(ctx, OpBranchDelete, repo)
	defer cancel()
	if err != nil {
		return res, err
	}
	info, err := o.repoInfo(ctx, tok, repo)
	if err != nil {
		return res, err
	}
	if branch == info.DefaultBranch {
		return res, fmt.Errorf("branch %q is the default branch", branch)
	}
	sha, found, err := o.headSHA(ctx, tok, repo, branch)
	if err != nil {
		return res, err
	}
	if !found {
		return res, fmt.Errorf("branch %q does not exist on GitHub", branch)
	}
	if open, found, err := o.openPRFor(ctx, tok, repo, branch); err != nil {
		return res, err
	} else if found {
		return res, fmt.Errorf("branch %q has an open PR #%d; merge or close it first", branch, open.Number)
	}
	if n, found, err := o.openPRInto(ctx, tok, repo, branch); err != nil {
		return res, err
	} else if found {
		return res, fmt.Errorf("branch %q is the base of open PR #%d; deleting it would close that PR", branch, n)
	}
	mergedBy, err = o.mergedBy(ctx, tok, repo, branch, sha, info.DefaultBranch)
	if err != nil {
		return res, err
	}
	if o.Refs == nil {
		return res, fmt.Errorf("branch_delete is not configured")
	}
	if err := o.Refs.DeleteBranch(ctx, repo, branch, sha, tok); err != nil {
		return res, err
	}
	return BranchDeleteResult{OK: true, Repo: repo, Branch: branch, SHA: sha, MergedBy: mergedBy}, nil
}

// mergedBy says how sha is in the default branch, or refuses. A PR merged
// with that exact head is checked first: comparing a branch that diverged
// from the default branch can return a diff past the API's size cap.
func (o *Ops) mergedBy(ctx context.Context, tok, repo, branch, sha, def string) (string, error) {
	owner := repo[:strings.IndexByte(repo, '/')]
	q := url.Values{"state": {"closed"}, "head": {owner + ":" + branch}, "base": {def}, "per_page": {strconv.Itoa(maxClosedPRs)}}
	var closed []ghClosedPull
	if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &closed); err != nil {
		return "", fmt.Errorf("list closed PRs: %w", err)
	}
	for _, p := range closed {
		if p.MergedAt != nil && *p.MergedAt != "" && p.Head.Ref == branch && p.Head.SHA == sha && p.Base.Ref == def &&
			p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, repo) {
			return fmt.Sprintf("pr #%d", p.Number), nil
		}
	}
	var cmp struct {
		Status string `json:"status"`
	}
	q = url.Values{"per_page": {"1"}}
	// heads/ pins the branch: a bare name resolves a tag of the same name first.
	if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/compare/heads/"+escapeRef(def)+"..."+sha+"?"+q.Encode(), nil, &cmp); err != nil {
		return "", fmt.Errorf("branch %q is not merged into %s, or that could not be checked (%v); nothing deleted", branch, def, err)
	}
	switch cmp.Status {
	case "identical", "behind":
		return "ancestor", nil
	}
	return "", fmt.Errorf("branch %q has commits that are not in %s and no PR into %s was merged at its head %s; nothing deleted", branch, def, def, sha)
}

// openPRInto returns an open PR whose base is branch.
func (o *Ops) openPRInto(ctx context.Context, tok, repo, branch string) (int, bool, error) {
	q := url.Values{"state": {"open"}, "base": {branch}, "per_page": {"1"}}
	var open []ghPull
	if err := o.API.do(ctx, tok, http.MethodGet, "/repos/"+repo+"/pulls?"+q.Encode(), nil, &open); err != nil {
		return 0, false, fmt.Errorf("list PRs into %s: %w", branch, err)
	}
	if len(open) > 0 {
		return open[0].Number, true, nil
	}
	return 0, false, nil
}
