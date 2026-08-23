package clitest

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

func TestExecCapturesBothStreamsAndTheError(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{Use: "go", RunE: func(cmd *cobra.Command, args []string) error {
		cmd.Print("out:", args[0])
		cmd.PrintErr(" err")
		return errors.New("boom")
	}})
	root.SilenceUsage, root.SilenceErrors = true, true
	out, err := Exec(t, root, "go", "x")
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
	if out != "out:x err" {
		t.Fatalf("out = %q", out)
	}
}

func TestNames(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{Use: "a"}, &cobra.Command{Use: "b [X]"})
	got := Names(root)
	if !got["a"] || !got["b"] || len(got) != 2 {
		t.Fatalf("names = %v", got)
	}
}

// recorder is a testing.TB that records Fatalf instead of stopping the test.
type recorder struct {
	testing.TB
	failed bool
}

func (r *recorder) Helper()               {}
func (r *recorder) Fatalf(string, ...any) { r.failed = true }

func TestWantErrIs(t *testing.T) {
	target := errors.New("target")
	for _, tc := range []struct {
		name string
		err  error
		fail bool
	}{
		{"wrapped", fmt.Errorf("outer: %w", target), false},
		{"other", errors.New("other"), true},
		{"nil", nil, true},
	} {
		r := &recorder{TB: t}
		WantErrIs(r, tc.err, target)
		if r.failed != tc.fail {
			t.Errorf("%s: failed = %v, want %v", tc.name, r.failed, tc.fail)
		}
	}
}

func TestWantErrContaining(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		subs []string
		fail bool
	}{
		{"all present", errors.New("a b c"), []string{"a", "c"}, false},
		{"one missing", errors.New("a b c"), []string{"a", "z"}, true},
		{"nil", nil, []string{"a"}, true},
	} {
		r := &recorder{TB: t}
		WantErrContaining(r, tc.err, tc.subs...)
		if r.failed != tc.fail {
			t.Errorf("%s: failed = %v, want %v", tc.name, r.failed, tc.fail)
		}
	}
}
