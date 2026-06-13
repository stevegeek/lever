package cli

import (
	"bytes"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := out.String(); got != Version+"\n" {
		t.Fatalf("got %q want %q", got, Version+"\n")
	}
}
