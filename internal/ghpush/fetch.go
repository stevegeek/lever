package ghpush

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/fsutil"
)

// OpFetch hands the agent a branch from GitHub as a git bundle: the read side
// of push. The agent can then merge GitHub's main (or a branch someone else
// pushed) and resolve conflicts itself, without ever holding a credential.
const OpFetch = "fetch"

// FetchResult is what fetch returns.
type FetchResult struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	Bundle string `json:"bundle"` // path in the container
	Bytes  int64  `json:"bytes"`
	Usage  string `json:"usage"`
}

// Fetch fetches refs/heads/<branch> from GitHub with a contents: read token
// and writes it as a full bundle (one ref, refs/heads/<branch>) to
// BundleDir/<bundle> in the tree, replacing any file of that name. The
// GitHub objects land only in a per-call repo borrowing the mirror's objects;
// the shared mirror still receives only main.
func (p *Pusher) Fetch(ctx context.Context, caller, repo, branch, bundle string) (res FetchResult, err error) {
	defer func() {
		decision, detail := "allow", ""
		if err != nil {
			decision, detail = "deny", err.Error()
		}
		p.Log.Info("github.fetch", "caller", caller, "repo", repo, "branch", branch, "bundle", bundle,
			"sha", res.SHA, "bytes", res.Bytes, "decision", decision, "detail", detail)
		if err != nil {
			err = p.hidePaths(err)
		}
	}()
	if p.Scoped == nil {
		return FetchResult{}, errors.New("fetch is not configured (no scoped token source)")
	}
	if err := ValidateRepo(repo, p.Repos); err != nil {
		return FetchResult{}, err
	}
	if err := ValidateFetchBranch(branch); err != nil {
		return FetchResult{}, err
	}
	if err := ValidateBundleName(bundle); err != nil {
		return FetchResult{}, err
	}
	if p.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Deadline)
		defer cancel()
	}
	unlock, err := p.lock(ctx, repo)
	if err != nil {
		return FetchResult{}, err
	}
	defer unlock()

	tmp, err := os.MkdirTemp(filepath.Join(p.State, "tmp"), "fetch-")
	if err != nil {
		return FetchResult{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	mirror, err := p.ensureMirror(ctx, repo)
	if err != nil {
		return FetchResult{}, err
	}
	token, err := p.Scoped.TokenFor(ctx, repo, OpPermissions[OpFetch])
	if err != nil {
		return FetchResult{}, err
	}
	remote := p.BaseURL + "/" + repo
	auth := &Auth{BaseURL: p.BaseURL, Token: token}
	if _, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror, Auth: auth}, "fetch", "--no-tags", "--no-write-fetch-head", remote, "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return FetchResult{}, fmt.Errorf("refresh mirror: %w", err)
	}
	work, err := p.newWorkRepo(ctx, tmp, mirror)
	if err != nil {
		return FetchResult{}, err
	}
	ref := "refs/heads/" + branch
	if _, err := p.Git.Run(ctx, work, RunOpts{GitDir: work, Auth: auth}, "fetch", "--no-tags", "--no-write-fetch-head", remote, "+"+ref+":"+ref); err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			return FetchResult{}, fmt.Errorf("branch %q does not exist on %s", branch, repo)
		}
		return FetchResult{}, fmt.Errorf("fetch %s: %w", branch, err)
	}
	sha, err := p.Git.Run(ctx, work, RunOpts{GitDir: work}, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return FetchResult{}, fmt.Errorf("fetched head is not a commit: %w", err)
	}
	// Estimate before building: the bundle packs the branch's whole history,
	// so a branch over the cap would otherwise fill the state disk first.
	// The estimate is charged to the caller's budget up front, refused or
	// not, so repeating a doomed fetch is not free.
	est, err := p.Git.Run(ctx, work, RunOpts{GitDir: work}, "rev-list", "--objects", "--disk-usage", ref)
	if err != nil {
		return FetchResult{}, fmt.Errorf("size of %s: %w", branch, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(est), 10, 64)
	if err != nil || size < 0 {
		return FetchResult{}, fmt.Errorf("size of %s: unreadable estimate", branch)
	}
	if err := p.charge(caller, min(size, p.MaxBundle)); err != nil {
		return FetchResult{}, err
	}
	if size > p.MaxBundle {
		return FetchResult{}, fmt.Errorf("%s holds about %d bytes of history, over the %d byte cap; a repository bigger than -max-bundle cannot be fetched", branch, size, p.MaxBundle)
	}
	out := filepath.Join(tmp, "out.bundle")
	if _, err := p.Git.Run(ctx, work, RunOpts{GitDir: work}, "bundle", "create", "--quiet", out, ref); err != nil {
		return FetchResult{}, fmt.Errorf("bundle create: %w", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return FetchResult{}, err
	}
	if fi.Size() > p.MaxBundle {
		return FetchResult{}, fmt.Errorf("bundle of %s is %d bytes, over the %d byte cap", branch, fi.Size(), p.MaxBundle)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return FetchResult{}, err
	}
	// Renamed over whatever the agent left at that name, never opened: a
	// planted link or FIFO cannot redirect the write.
	if err := fsutil.ReplaceInTreeNoLinks(p.Tree, path.Join(BundleDir, bundle), data, 0o755, 0o644); err != nil {
		return FetchResult{}, fmt.Errorf("write bundle %s: %w", bundle, err)
	}
	dst := "/workspace/" + BundleDir + "/" + bundle
	return FetchResult{
		Repo: repo, Branch: branch, SHA: strings.TrimSpace(sha), Bundle: dst, Bytes: fi.Size(),
		// "+": GitHub's branch may have been rewritten since the last fetch.
		Usage: fmt.Sprintf("git fetch %s +%s:refs/remotes/origin/%s", dst, ref, branch),
	}, nil
}

// ValidateFetchBranch accepts any branch name a push could make, plus main
// and the like: reading needs no prefix.
func ValidateFetchBranch(b string) error {
	switch {
	case len(b) == 0 || len(b) > 200:
		return fmt.Errorf("branch name must be 1-200 characters")
	case !branchRE.MatchString(b):
		return fmt.Errorf("branch %q: only letters, digits, '.', '_', '-' and '/' are allowed", b)
	case strings.Contains(b, ".."):
		return fmt.Errorf("branch %q must not contain '..'", b)
	case b == "HEAD":
		return fmt.Errorf("branch %q: name a branch", b)
	}
	for _, c := range strings.Split(b, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasPrefix(c, "-") || strings.HasSuffix(c, ".") || strings.HasSuffix(c, ".lock") {
			return fmt.Errorf("branch %q has an invalid path component %q", b, c)
		}
	}
	return nil
}
