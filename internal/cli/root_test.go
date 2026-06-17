package cli

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

func names(root *cobra.Command) map[string]bool {
	m := map[string]bool{}
	for _, c := range root.Commands() {
		m[c.Name()] = true
	}
	return m
}

func TestHostRootHasProvisioningOnly(t *testing.T) {
	n := names(NewHostRoot())
	for _, want := range []string{"up", "apply", "down", "doctor", "provision", "version"} {
		if !n[want] {
			t.Errorf("host root missing %q", want)
		}
	}
	for _, unwanted := range []string{"agent", "msg", "watch"} {
		if n[unwanted] {
			t.Errorf("host root should not have %q", unwanted)
		}
	}
}

func TestManagerRootHasOrchestrationOnly(t *testing.T) {
	n := names(NewManagerRoot())
	for _, want := range []string{"agent", "msg", "watch", "version"} {
		if !n[want] {
			t.Errorf("manager root missing %q", want)
		}
	}
	for _, unwanted := range []string{"up", "apply", "down", "doctor", "provision"} {
		if n[unwanted] {
			t.Errorf("manager root should not have %q", unwanted)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	root := NewHostRoot()
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
