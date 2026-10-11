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
	"unicode"
	"unicode/utf8"
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

var (
	numberRE = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,99}$`)
	shaRE    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// TitleMax is the longest PR title, in characters.
const TitleMax = 256

// ValidateNumber accepts a PR number: a positive decimal integer.
func ValidateNumber(n string) error {
	if !numberRE.MatchString(n) {
		return fmt.Errorf("number %q: want a positive PR number such as 12", n)
	}
	return nil
}

// ValidateTitle accepts a PR title: 1 to TitleMax characters of valid
// UTF-8, not blank, and no control or bidirectional formatting character.
func ValidateTitle(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("title: not valid UTF-8")
	}
	if n := utf8.RuneCountInString(s); n == 0 || n > TitleMax || strings.TrimSpace(s) == "" {
		return fmt.Errorf("title must be 1-%d characters and not blank, got %d", TitleMax, n)
	}
	for _, r := range s {
		if badTextRune(r, false) {
			return fmt.Errorf("title: control or formatting character %U refused", r)
		}
	}
	return nil
}

// ValidateText accepts a PR body or release notes: at most max bytes of
// valid UTF-8, no control character except newline and tab, and no
// bidirectional or invisible formatting character (it could make the
// published text read differently from what was reviewed).
func ValidateText(what, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s: %d bytes, max %d", what, len(s), max)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s: not valid UTF-8", what)
	}
	for _, r := range s {
		if badTextRune(r, true) {
			return fmt.Errorf("%s: control or formatting character %U refused (only newline and tab are allowed)", what, r)
		}
	}
	return nil
}

func badTextRune(r rune, multiline bool) bool {
	if multiline && (r == '\n' || r == '\t') {
		return false
	}
	if unicode.IsControl(r) || r == 0x2028 || r == 0x2029 {
		return true
	}
	// Format characters (bidirectional controls, zero-width and other
	// invisible characters) except the zero-width joiner that emoji
	// sequences use.
	return unicode.Is(unicode.Cf, r) && r != 0x200d
}

// ValidateTagName is the charset and ref-name check of a release tag, made
// before the configured pattern: no '/', no "..", no ".lock" suffix, no
// leading '-' or '.'.
func ValidateTagName(t string) error {
	if !tagRE.MatchString(t) || strings.Contains(t, "..") || strings.HasSuffix(t, ".lock") || strings.HasSuffix(t, ".") {
		return fmt.Errorf("tag %q: want letters, digits, '.', '_', '+' and '-' (1-100 characters), such as v1.2.3", t)
	}
	return nil
}
