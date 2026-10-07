package ghpush

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

var (
	// ErrBranchMoved: GitHub refused a non-fast-forward update.
	ErrBranchMoved = errors.New("branch moved on GitHub; push to a new branch name (no force push)")
	// ErrBusy: another push to the same repo held the mirror too long.
	ErrBusy = errors.New("another push to this repo is in progress; retry shortly")
)

// Pusher pushes agent branches from agent-written bundles.
type Pusher struct {
	Tree      string // instance tree (absolute)
	State     string // private state dir (absolute, 0700)
	Prefix    string // required branch prefix, e.g. "agent/"
	BaseURL   string // https://github.com
	Repos     map[string]bool
	MaxBundle int64
	LockWait  time.Duration
	Tokens    TokenSource
	Git       Git
	Log       *slog.Logger
	// Deadline bounds one whole call, lock wait included (0 = none). Each
	// git call also has Git.Timeout.
	Deadline time.Duration
	// Limits bound what the import inflates (checked before any git call).
	Limits PackLimits
	// ImportBudget caps the bundle bytes one caller may import per hour,
	// refused pushes included (0 = no cap).
	ImportBudget int64

	locks   sync.Map // repo → chan struct{} (capacity 1)
	spentMu sync.Mutex
	spent   map[string][]spend
	now     func() time.Time // tests only
}

// Result is what the agent gets back.
type Result struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch"`
	OldSHA     string `json:"old_sha"`
	NewSHA     string `json:"new_sha"`
	CompareURL string `json:"compare_url"`
}

func (p *Pusher) lock(ctx context.Context, repo string) (func(), error) {
	v, _ := p.locks.LoadOrStore(repo, make(chan struct{}, 1))
	ch := v.(chan struct{})
	t := time.NewTimer(p.LockWait)
	defer t.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Push runs the whole flow (spec §4.2) and writes one audit line.
func (p *Pusher) Push(ctx context.Context, caller, repo, branch, bundle string) (res Result, err error) {
	var bundleSHA string
	defer func() {
		decision, detail := "allow", ""
		if err != nil {
			decision, detail = "deny", err.Error()
		}
		p.Log.Info("github.push", "caller", caller, "repo", repo, "branch", branch,
			"old", res.OldSHA, "new", res.NewSHA, "bundle_sha256", bundleSHA, "decision", decision, "detail", detail)
		// The host log keeps the paths; the agent gets placeholders.
		if err != nil {
			err = p.hidePaths(err)
		}
	}()
	if err := ValidateRepo(repo, p.Repos); err != nil {
		return Result{}, err
	}
	if err := ValidateBranch(branch, p.Prefix); err != nil {
		return Result{}, err
	}
	if err := ValidateBundleName(bundle); err != nil {
		return Result{}, err
	}
	if p.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Deadline)
		defer cancel()
	}
	unlock, err := p.lock(ctx, repo)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	tmp, err := os.MkdirTemp(filepath.Join(p.State, "tmp"), "push-")
	if err != nil {
		return Result{}, fmt.Errorf("temp dir: %w", err)
	}
	// The bundle's objects live only in tmp: removing it on every path
	// (success, refusal, error, panic) keeps them off the host disk.
	defer os.RemoveAll(tmp)
	copyPath := filepath.Join(tmp, "in.bundle")
	var size int64
	if bundleSHA, size, err = p.copyBundle(bundle, copyPath); err != nil {
		return Result{}, err
	}
	if err := p.charge(caller, size); err != nil {
		return Result{}, err
	}
	if err := scanBundle(ctx, copyPath, p.Limits); err != nil {
		return Result{}, err
	}

	mirror, err := p.ensureMirror(ctx, repo)
	if err != nil {
		return Result{}, err
	}
	remote := p.BaseURL + "/" + repo
	if _, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror}, "fetch", "--no-tags", "--no-write-fetch-head", remote, "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return Result{}, fmt.Errorf("refresh mirror: %w", err)
	}
	// The mirror only ever receives main from GitHub. The bundle goes into a
	// per-call repo that borrows the mirror's objects (alternates), so a
	// refused bundle never lands in the shared mirror, and one caller's
	// objects are never another caller's prerequisites.
	work, err := p.newWorkRepo(ctx, tmp, mirror)
	if err != nil {
		return Result{}, err
	}
	if _, err := p.Git.Run(ctx, work, RunOpts{GitDir: work}, "bundle", "verify", "--quiet", copyPath); err != nil {
		return Result{}, fmt.Errorf("bundle does not verify against %s main (missing prerequisites?): %w", repo, err)
	}
	heads, err := p.Git.Run(ctx, work, RunOpts{GitDir: work}, "bundle", "list-heads", copyPath)
	if err != nil {
		return Result{}, fmt.Errorf("bundle list-heads: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(heads), "\n")
	if len(lines) != 1 || strings.TrimSpace(lines[0]) == "" {
		return Result{}, fmt.Errorf("bundle must carry exactly one ref (refs/heads/%s), found %d", branch, len(lines))
	}
	if f := strings.Fields(lines[0]); len(f) != 2 || f[1] != "refs/heads/"+branch {
		return Result{}, fmt.Errorf("bundle must carry exactly one ref, refs/heads/%s; found %q", branch, lines[0])
	}

	tmpRef := "refs/tmp/import"
	if _, err := p.Git.Run(ctx, work, RunOpts{GitDir: work, AllowFile: true}, "fetch", "--no-tags", "--no-write-fetch-head", copyPath, "+refs/heads/"+branch+":"+tmpRef); err != nil {
		return Result{}, fmt.Errorf("import bundle: %w", err)
	}
	// The head must be a commit itself: an annotated tag (or any other
	// object) would peel to a commit and push a different object than the
	// one checked.
	rawSHA, err := p.Git.Run(ctx, work, RunOpts{GitDir: work}, "rev-parse", "--verify", tmpRef)
	if err != nil {
		return Result{}, fmt.Errorf("bundle head is missing: %w", err)
	}
	newSHA, err := p.Git.Run(ctx, work, RunOpts{GitDir: work}, "rev-parse", "--verify", tmpRef+"^{commit}")
	if err != nil || strings.TrimSpace(rawSHA) != strings.TrimSpace(newSHA) {
		return Result{}, fmt.Errorf("bundle head is not a commit (an annotated tag or another object): refused")
	}
	res = Result{Repo: repo, Branch: branch, NewSHA: strings.TrimSpace(newSHA),
		CompareURL: p.BaseURL + "/" + repo + "/compare/main..." + branch}

	token, err := p.Tokens.Token(ctx, repo)
	if err != nil {
		return res, err
	}
	auth := &Auth{BaseURL: p.BaseURL, Token: token}
	if out, err := p.Git.Run(ctx, work, RunOpts{GitDir: work, Auth: auth}, "ls-remote", remote, "refs/heads/"+branch); err != nil {
		return res, fmt.Errorf("read remote branch: %w", err)
	} else {
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[1] == "refs/heads/"+branch {
				res.OldSHA = f[0]
				break
			}
		}
	}
	if _, err := p.Git.Run(ctx, work, RunOpts{GitDir: work, Auth: auth}, "push", remote, res.NewSHA+":refs/heads/"+branch); err != nil {
		// Without --porcelain, git writes " ! [rejected] … (non-fast-forward)"
		// to stderr, which Run puts in the error.
		if s := err.Error(); strings.Contains(s, "non-fast-forward") || strings.Contains(s, "fetch first") || strings.Contains(s, "[rejected]") {
			return res, fmt.Errorf("%w: %s", ErrBranchMoved, err)
		}
		return res, fmt.Errorf("push: %w", err)
	}
	return res, nil
}

// copyBundle copies the agent's bundle into a private file: no symlink on any
// path component, no hard link, at most MaxBundle bytes. git then reads only
// the private copy, so the agent cannot swap the file mid-flow.
func (p *Pusher) copyBundle(name, dst string) (string, int64, error) {
	f, _, err := fsutil.OpenInTreeNoLinks(p.Tree, path.Join(BundleDir, name), p.MaxBundle)
	if err != nil {
		if errors.Is(err, fsutil.ErrSymlink) || errors.Is(err, fsutil.ErrHardLink) {
			return "", 0, fmt.Errorf("bundle %s: refused (a symbolic or hard link): %w", name, err)
		}
		if errors.Is(err, fsutil.ErrFileTooLarge) {
			return "", 0, fmt.Errorf("bundle %s: too big (max %d bytes): %w", name, p.MaxBundle, err)
		}
		return "", 0, fmt.Errorf("bundle %s: %w", name, err)
	}
	defer f.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, p.MaxBundle+1))
	if err != nil {
		return "", 0, fmt.Errorf("copy bundle: %w", err)
	}
	if n > p.MaxBundle {
		return "", 0, fmt.Errorf("bundle %s: too big (max %d bytes)", name, p.MaxBundle)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ensureMirror returns the repo's bare, hook-less mirror, creating it once.
func (p *Pusher) ensureMirror(ctx context.Context, repo string) (string, error) {
	dir := filepath.Join(p.State, "mirrors", filepath.FromSlash(repo)+".git")
	if fi, err := os.Lstat(dir); err == nil {
		// A half-initialised or foreign directory is rebuilt, never trusted.
		if fi.IsDir() {
			out, err := p.Git.Run(ctx, p.State, RunOpts{GitDir: dir}, "rev-parse", "--is-bare-repository")
			if err == nil && strings.TrimSpace(out) == "true" {
				return dir, nil
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("remove broken mirror: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if _, err := p.Git.Run(ctx, p.State, RunOpts{}, "init", "--bare", "--template=", "-b", "main", dir); err != nil {
		return "", fmt.Errorf("create mirror: %w", err)
	}
	return dir, nil
}

// newWorkRepo creates the per-call bare repo in tmp. It reads the mirror's
// objects through objects/info/alternates and writes only its own.
func (p *Pusher) newWorkRepo(ctx context.Context, tmp, mirror string) (string, error) {
	dir := filepath.Join(tmp, "work.git")
	if _, err := p.Git.Run(ctx, tmp, RunOpts{}, "init", "--bare", "--template=", "-b", "main", dir); err != nil {
		return "", fmt.Errorf("create work repo: %w", err)
	}
	info := filepath.Join(dir, "objects", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.Join(mirror, "objects")+"\n"), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

var tmpPathRE = regexp.MustCompile(`<state>/tmp/push-[0-9]+`)

// hidePaths replaces the host paths (state dir, per-call temp dir, git home,
// tree) in err's text with placeholders. errors.Is still sees the cause.
func (p *Pusher) hidePaths(err error) error {
	msg := err.Error()
	var pairs [][2]string
	for _, d := range []struct{ path, name string }{{p.Git.Home, "<git-home>"}, {p.State, "<state>"}, {p.Tree, "<tree>"}} {
		if d.path == "" {
			continue
		}
		pairs = append(pairs, [2]string{d.path, d.name})
		if real, e := filepath.EvalSymlinks(d.path); e == nil && real != d.path {
			pairs = append(pairs, [2]string{real, d.name})
		}
	}
	// Longest first: the git home sits inside the state dir.
	slices.SortStableFunc(pairs, func(a, b [2]string) int { return len(b[0]) - len(a[0]) })
	for _, pr := range pairs {
		msg = strings.ReplaceAll(msg, pr[0], pr[1])
	}
	msg = tmpPathRE.ReplaceAllString(msg, "<tmp>")
	if msg == err.Error() {
		return err
	}
	return &hiddenPathsError{msg: msg, err: err}
}

type hiddenPathsError struct {
	msg string
	err error
}

func (e *hiddenPathsError) Error() string { return e.msg }
func (e *hiddenPathsError) Unwrap() error { return e.err }

// importWindow is the period the per-caller ImportBudget covers.
const importWindow = time.Hour

type spend struct {
	at time.Time
	n  int64
}

// charge books n imported bundle bytes to caller, or refuses when the
// caller's bytes in the last importWindow would exceed ImportBudget. Refused
// pushes count too: the cost is the import, not the push.
func (p *Pusher) charge(caller string, n int64) error {
	if p.ImportBudget <= 0 {
		return nil
	}
	now := time.Now()
	if p.now != nil {
		now = p.now()
	}
	p.spentMu.Lock()
	defer p.spentMu.Unlock()
	if p.spent == nil {
		p.spent = map[string][]spend{}
	}
	var kept []spend
	var total int64
	for _, s := range p.spent[caller] {
		if now.Sub(s.at) < importWindow {
			kept = append(kept, s)
			total += s.n
		}
	}
	if total+n > p.ImportBudget {
		p.spent[caller] = kept
		return fmt.Errorf("import budget: %s imported %d bytes in the last hour (max %d); retry later", caller, total, p.ImportBudget)
	}
	p.spent[caller] = append(kept, spend{now, n})
	return nil
}
