package ghpush

import (
	"errors"
	"testing"
)

func TestValidateBranch(t *testing.T) {
	good := []string{"agent/x", "agent/fix-123", "agent/a.b_c/d", "agent/main2"}
	bad := []string{
		"", "main", "master", "HEAD", "agent/", "agent/main", "agent/master", "agent/HEAD",
		"-agent/x", "agent/-x/../y", "agent//x", "agent/x/", "agent/.x", "agent/x.", "agent/x.lock",
		"agent/x.lock/y", "agent/@{-1}", "agent/x y", "agent/x~1", "agent/x^", "agent/x:y",
		"agent/x?", "agent/x*", "agent/x[", `agent/x\y`, "agent/a..b", "agent/./x", "other/x", "agent/\x01",
		"refs/heads/agent/x", "agent/" + string(make([]byte, 200)),
	}
	for _, b := range good {
		if err := ValidateBranch(b, "agent/"); err != nil {
			t.Errorf("%q should be valid: %v", b, err)
		}
	}
	for _, b := range bad {
		if err := ValidateBranch(b, "agent/"); err == nil {
			t.Errorf("%q should be refused", b)
		}
	}
}

func TestValidatePrefix(t *testing.T) {
	for _, p := range []string{"agent/", "lever-dev/"} {
		if err := ValidatePrefix(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	for _, p := range []string{"", "agent", "/", "-a/", "a//", "main/"} {
		if err := ValidatePrefix(p); err == nil {
			t.Errorf("%q should be refused", p)
		}
	}
}

func TestValidateBundleName(t *testing.T) {
	for _, n := range []string{"x.bundle", "agent-x_1.2.bundle"} {
		if err := ValidateBundleName(n); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"", ".bundle", "x", "x.zip", "a/x.bundle", "../x.bundle", ".x.bundle", "x .bundle", string(make([]byte, 94)) + ".bundle"} {
		if err := ValidateBundleName(n); err == nil {
			t.Errorf("%q should be refused", n)
		}
	}
}

func TestValidateRepo(t *testing.T) {
	allowed := map[string]bool{"stevegeek/lever": true}
	if err := ValidateRepo("stevegeek/lever", allowed); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"", "stevegeek/other", "stevegeek/lever/x", "../lever", "stevegeek/lever.git"} {
		if err := ValidateRepo(r, allowed); err == nil {
			t.Errorf("%q should be refused", r)
		}
	}
}

func TestValidatePRText(t *testing.T) {
	for _, c := range []struct {
		title, body string
		want        error
	}{
		{"t", "cc @stevegeek/maintainers", ErrTeamMention},
		{"ping @org/team", "", ErrTeamMention},
		{"t", "(@Org-1/team.x)", ErrTeamMention},
		{"t", "@org/team", ErrTeamMention},
		{"Fixes #12", "", ErrClosingKeyword},
		{"t", "This closes #3.", ErrClosingKeyword},
		{"t", "close #3", ErrClosingKeyword},
		{"t", "CLOSED #3", ErrClosingKeyword},
		{"t", "fix #1", ErrClosingKeyword},
		{"t", "fixed #1", ErrClosingKeyword},
		{"t", "Resolve owner/repo#44", ErrClosingKeyword},
		{"t", "resolves: #44", ErrClosingKeyword},
		{"t", "the fix: #3", ErrClosingKeyword},
		{"t", "Fixes\u00a0#1", ErrClosingKeyword},
		{"t", "closes\u2003#1", ErrClosingKeyword},
		{"t", "resolved\n#44", ErrClosingKeyword},
		{"t", "fixes https://github.com/o/r/issues/9", ErrClosingKeyword},
		{"t", "@a @b @c @d @e @f", ErrTooManyMentions},
		{"@a @b @c", "@d @e @f", ErrTooManyMentions},
	} {
		err := ValidatePRText(c.title, c.body)
		if !errors.Is(err, c.want) {
			t.Errorf("%q / %q: got %v, want %v", c.title, c.body, err, c.want)
		}
	}
	for _, c := range []struct{ title, body string }{
		{"t", "refs #12, see #13"},
		{"Add a prefix #3 option", "a fixture #3 is not a keyword"},
		{"t", "mail me@example.com or a.b@host/path"},
		{"t", "@a @b @c @d @e and @A again"},
		{"t", "closest #3; fixture #4; unresolved #5"},
		{"Fix the parser", "Fixes the bug in #12's area"},
	} {
		if err := ValidatePRText(c.title, c.body); err != nil {
			t.Errorf("%q / %q: %v", c.title, c.body, err)
		}
	}
}
