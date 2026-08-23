package clitest

import (
	"errors"
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
