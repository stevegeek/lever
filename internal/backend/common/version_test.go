package common

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

var testVersionRe = regexp.MustCompile(`Version:\s*(\d+)\.(\d+)\.(\d+)`)

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Version: 2.1.1 (x)", true},
		{"Version: 2.1.0", false},
		{"Version: 2.0.9", false},
		{"Version: 1.9.9", false},
		{"Version: 3.0.0", true},
		{"Version: 2.2.0", true},
		{"Version: 10.0.0", true},
	}
	for _, c := range cases {
		f := exec.NewFakeRunner()
		f.Script("tool version", exec.Result{Stdout: c.out + "\n"})
		ok, got, err := VersionAtLeast(context.Background(), f, []string{"tool", "version"}, testVersionRe, 2, 1, 1)
		if err != nil {
			t.Fatalf("%q: %v", c.out, err)
		}
		if ok != c.want {
			t.Errorf("%q: ok=%v want %v (got %q)", c.out, ok, c.want, got)
		}
		if !strings.HasPrefix(c.out, "Version: "+got) {
			t.Errorf("%q: parsed %q", c.out, got)
		}
	}
}

func TestVersionAtLeastErrors(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("tool version", exec.Result{Stdout: "garbage\n"})
	ok, got, err := VersionAtLeast(context.Background(), f, []string{"tool", "version"}, testVersionRe, 1, 0, 0)
	if err == nil || ok || got != "garbage" || !strings.Contains(err.Error(), "tool version: could not parse") {
		t.Fatalf("parse failure: ok=%v got=%q err=%v", ok, got, err)
	}
	_, _, err = VersionAtLeast(context.Background(), exec.NewFakeRunner(), []string{"tool", "version"}, testVersionRe, 1, 0, 0)
	if err == nil || !strings.HasPrefix(err.Error(), "tool version: ") {
		t.Fatalf("run failure: %v", err)
	}
}
