package ghpush

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
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

	locks sync.Map // repo → chan struct{} (capacity 1)
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
	unlock, err := p.lock(ctx, repo)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	tmp, err := os.MkdirTemp(filepath.Join(p.State, "tmp"), "push-")
	if err != nil {
		return Result{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	copyPath := filepath.Join(tmp, "in.bundle")
	if bundleSHA, err = p.copyBundle(bundle, copyPath); err != nil {
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
	if _, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror}, "bundle", "verify", "--quiet", copyPath); err != nil {
		return Result{}, fmt.Errorf("bundle does not verify against %s main (missing prerequisites?): %w", repo, err)
	}
	heads, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror}, "bundle", "list-heads", copyPath)
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

	id, err := randomID()
	if err != nil {
		return Result{}, err
	}
	tmpRef := "refs/tmp/" + id
	defer p.Git.Run(context.Background(), mirror, RunOpts{GitDir: mirror}, "update-ref", "-d", tmpRef) //nolint:errcheck
	if _, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror, AllowFile: true}, "fetch", "--no-tags", "--no-write-fetch-head", copyPath, "+refs/heads/"+branch+":"+tmpRef); err != nil {
		return Result{}, fmt.Errorf("import bundle: %w", err)
	}
	newSHA, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror}, "rev-parse", "--verify", tmpRef+"^{commit}")
	if err != nil {
		return Result{}, fmt.Errorf("bundle head is not a commit: %w", err)
	}
	res = Result{Repo: repo, Branch: branch, NewSHA: strings.TrimSpace(newSHA),
		CompareURL: p.BaseURL + "/" + repo + "/compare/main..." + branch}

	token, err := p.Tokens.Token(ctx, repo)
	if err != nil {
		return res, err
	}
	auth := &Auth{BaseURL: p.BaseURL, Token: token}
	if out, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror, Auth: auth}, "ls-remote", remote, "refs/heads/"+branch); err != nil {
		return res, fmt.Errorf("read remote branch: %w", err)
	} else if f := strings.Fields(out); len(f) >= 1 {
		res.OldSHA = f[0]
	}
	if _, err := p.Git.Run(ctx, mirror, RunOpts{GitDir: mirror, Auth: auth}, "push", remote, tmpRef+":refs/heads/"+branch); err != nil {
		// Without --porcelain, git writes " ! [rejected] … (non-fast-forward)"
		// to stderr, which Run puts in the error.
		if s := err.Error(); strings.Contains(s, "non-fast-forward") || strings.Contains(s, "fetch first") || strings.Contains(s, "[rejected]") {
			return res, ErrBranchMoved
		}
		return res, fmt.Errorf("push: %w", err)
	}
	return res, nil
}

// copyBundle copies the agent's bundle into a private file: no symlink on any
// path component, no hard link, at most MaxBundle bytes. git then reads only
// the private copy, so the agent cannot swap the file mid-flow.
func (p *Pusher) copyBundle(name, dst string) (string, error) {
	f, _, err := fsutil.OpenInTreeNoLinks(p.Tree, path.Join(BundleDir, name), p.MaxBundle)
	if err != nil {
		if errors.Is(err, fsutil.ErrSymlink) || errors.Is(err, fsutil.ErrHardLink) {
			return "", fmt.Errorf("bundle %s: refused (a symbolic or hard link): %w", name, err)
		}
		if errors.Is(err, fsutil.ErrFileTooLarge) {
			return "", fmt.Errorf("bundle %s: too big (max %d bytes): %w", name, p.MaxBundle, err)
		}
		return "", fmt.Errorf("bundle %s: %w", name, err)
	}
	defer f.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer out.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(f, p.MaxBundle+1))
	if err != nil {
		return "", fmt.Errorf("copy bundle: %w", err)
	}
	if n > p.MaxBundle {
		return "", fmt.Errorf("bundle %s: too big (max %d bytes)", name, p.MaxBundle)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
