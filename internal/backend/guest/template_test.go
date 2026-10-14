package guest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// TestEnsureLeverTemplateWritesAnEmptyPrompt pins the two things the overlay
// depends on: the file it creates is EMPTY (scion emits --system-prompt only
// when the staged content is non-empty after TrimSpace, so content here would
// defeat the entire purpose), and it lands under a name that is NOT "default"
// (that name would suppress scion's automatic base-layer prepend and strip
// agents.md/home/skills from the chain).
func TestEnsureLeverTemplateWritesAnEmptyPrompt(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("orb", proc.Result{Stdout: "wrote"})
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

// TestEnsureLeverTemplateConvergesAgainstARealFile runs the guest script FOR
// REAL, with HOME pointed at a temp dir, because the whole convergence
// decision lives in that script and nowhere in Go; a fake runner would stay
// green if `-s` became `-e`.
//
// ABSENT creates an empty file and reports a change; CONVERGED touches
// nothing and reports none; NON-EMPTY truncates and reports a change.
func TestEnsureLeverTemplateConvergesAgainstARealFile(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH: this test runs the guest script rather than mocking it")
	}
	home := t.TempDir()
	// `env HOME=<tmp>` stands in for the guest prefix and runs the script
	// against a throwaway home.
	g := Guest{Host: proc.RealRunner{}, UserPrefix: []string{"env", "HOME=" + home}}
	prompt := filepath.Join(home, ".scion", "templates", LeverTemplateName, "system-prompt.md")

	changed, err := g.EnsureLeverTemplate(context.Background())
	if err != nil {
		t.Fatalf("EnsureLeverTemplate (absent): %v", err)
	}
	if !changed {
		t.Fatal("a missing system-prompt.md must be reported as a change")
	}
	fi, err := os.Stat(prompt)
	if err != nil {
		t.Fatalf("the overlay template was not created at %s: %v", prompt, err)
	}
	if fi.Size() != 0 {
		t.Fatalf("system-prompt.md is %d bytes; only an EMPTY file suppresses --system-prompt", fi.Size())
	}

	changed, err = g.EnsureLeverTemplate(context.Background())
	if err != nil {
		t.Fatalf("EnsureLeverTemplate (converged): %v", err)
	}
	if changed {
		t.Fatal("a second apply reported a change against an already-empty file — the operator would see the notice on every apply")
	}
	again, err := os.Stat(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !again.ModTime().Equal(fi.ModTime()) {
		t.Fatal("the converged file was rewritten: the script must DECIDE before it truncates, not truncate and then decide")
	}

	// Scion's own placeholder is the realistic non-empty content: this is the
	// state the overlay exists to correct.
	if err := os.WriteFile(prompt, []byte("# Placeholder\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = g.EnsureLeverTemplate(context.Background())
	if err != nil {
		t.Fatalf("EnsureLeverTemplate (non-empty): %v", err)
	}
	if !changed {
		t.Fatal("a non-empty system-prompt.md must be reported as a change")
	}
	if fi, err := os.Stat(prompt); err != nil || fi.Size() != 0 {
		t.Fatalf("a non-empty system-prompt.md was not truncated (stat %v, err %v) — agents would keep launching with the placeholder prompt", fi, err)
	}
}
