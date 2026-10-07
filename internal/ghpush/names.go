// Package ghpush is the host side of lever-tool-github: it validates an
// agent's push request, imports the agent's git bundle into a host-owned
// mirror, and pushes one agent branch to GitHub with a short-lived GitHub App
// token. The agent never holds a credential, and nothing here runs code from
// the agent's repository: the mirror is bare, hook-less and never checked out.
package ghpush

import (
	"fmt"
	"regexp"
	"strings"
)

// BundleDir is the tree-relative directory the agent writes bundles into
// (/workspace/.lever-files/github in the container).
const BundleDir = ".lever-files/github"

var (
	branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	bundleRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,92}\.bundle$`)
	repoRE   = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)
)

// ValidateRepo accepts only an owner/name the tool is configured for.
func ValidateRepo(repo string, allowed map[string]bool) error {
	if !repoRE.MatchString(repo) || !allowed[repo] {
		return fmt.Errorf("repo %q is not one this tool may push to", repo)
	}
	return nil
}

// ValidatePrefix checks the configured branch prefix at startup.
func ValidatePrefix(p string) error {
	if !strings.HasSuffix(p, "/") || len(p) < 2 || strings.HasPrefix(p, "-") || strings.HasPrefix(p, "/") ||
		strings.Contains(p, "//") || !branchRE.MatchString(p) || p == "main/" || p == "master/" {
		return fmt.Errorf("branch prefix %q invalid: want a name ending in / such as agent/", p)
	}
	return nil
}

// ValidateBranch is the whole branch check, in Go, before any git call: a
// conservative charset (so no @{, ~, ^, :, ?, *, [, \, space or control
// character can reach git), the required prefix, git's component rules, and
// no protected name after the prefix.
func ValidateBranch(b, prefix string) error {
	switch {
	case len(b) == 0 || len(b) > 200:
		return fmt.Errorf("branch name must be 1-200 characters")
	case !branchRE.MatchString(b):
		return fmt.Errorf("branch %q: only letters, digits, '.', '_', '-' and '/' are allowed", b)
	case !strings.HasPrefix(b, prefix) || len(b) == len(prefix):
		return fmt.Errorf("branch %q must start with %q and name something after it", b, prefix)
	case strings.Contains(b, ".."):
		return fmt.Errorf("branch %q must not contain '..'", b)
	}
	for _, c := range strings.Split(b, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasPrefix(c, "-") || strings.HasSuffix(c, ".") || strings.HasSuffix(c, ".lock") {
			return fmt.Errorf("branch %q has an invalid path component %q", b, c)
		}
	}
	switch strings.TrimPrefix(b, prefix) {
	case "main", "master", "HEAD":
		return fmt.Errorf("branch %q is protected", b)
	}
	return nil
}

// ValidateBundleName accepts one plain file name ending in .bundle.
func ValidateBundleName(n string) error {
	if !bundleRE.MatchString(n) {
		return fmt.Errorf("bundle %q: want a single file name like my-branch.bundle in %s", n, BundleDir)
	}
	return nil
}
