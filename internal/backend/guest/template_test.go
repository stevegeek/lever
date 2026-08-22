package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

// TestEnsureLeverTemplateWritesAnEmptyPrompt pins the two things the overlay
// depends on: the file it creates is EMPTY (scion emits --system-prompt only
// when the staged content is non-empty after TrimSpace, so content here would
// defeat the entire purpose), and it lands under a name that is NOT "default"
// (that name would suppress scion's automatic base-layer prepend and strip
// agents.md/home/skills from the chain).
func TestEnsureLeverTemplateWritesAnEmptyPrompt(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb", exec.Result{Stdout: "wrote"})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}}

	changed, err := g.EnsureLeverTemplate(context.Background())
	if err != nil {
		t.Fatalf("EnsureLeverTemplate: %v", err)
	}
	if !changed {
		t.Fatal("changed = false when the guest reported a write")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want exactly one guest call, got %d", len(f.Calls))
	}
	got := strings.Join(f.Calls[0].Args, " ")
	if LeverTemplateName == "default" {
		t.Fatal(`the overlay must NOT be named "default": GetTemplateChainInProject only prepends the stock default for OTHER names, so this would replace the template rather than layer on it`)
	}
	if !strings.Contains(got, ".scion/templates/"+LeverTemplateName) {
		t.Errorf("script does not target the overlay template dir: %s", got)
	}
	// `: >` truncates to empty. Anything that writes bytes (echo, cat, printf
	// with content) would make scion emit --system-prompt again.
	if !strings.Contains(got, ": >") {
		t.Errorf("script does not create an EMPTY system-prompt.md — an empty file is what suppresses the flag: %s", got)
	}
	if strings.Contains(got, "scion-agent.yaml") {
		t.Errorf("the overlay must not carry its own config; it inherits the default's: %s", got)
	}
}

// TestEnsureLeverTemplateIsQuietWhenConverged: a re-apply must not report a
// change, because the caller logs on change and the operator would otherwise
// see the same notice on every apply.
func TestEnsureLeverTemplateIsQuietWhenConverged(t *testing.T) {
	f := exec.NewFakeRunner()
	f.Script("orb", exec.Result{Stdout: ""})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}}
	changed, err := g.EnsureLeverTemplate(context.Background())
	if err != nil {
		t.Fatalf("EnsureLeverTemplate: %v", err)
	}
	if changed {
		t.Fatal("changed = true when the guest wrote nothing")
	}
}
