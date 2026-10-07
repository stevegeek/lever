package ghpush

import "testing"

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
