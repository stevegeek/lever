package host

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestPrintDoctorReportSanitizesRows: a detail or fix carrying terminal
// escapes (a compromised jail can put them in any string doctor relays)
// reaches the terminal stripped, and the failure count is unaffected.
func TestPrintDoctorReportSanitizesRows(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	checks := []checkResult{
		{"manager agent", true, "\"m\" is running (container \x1b]0;pwned\x07running)", ""},
		{"manager image", false, "was created on \x1b[2Jevil:latest", "run \x1b[1;31m`lever up --fresh`\x1b[0m\n\x1b[2K✓ fake row"},
	}
	if failed := printDoctorReport(cmd, checks); failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
	got := out.String()
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Fatalf("escape bytes reached the terminal:\n%q", got)
	}
	want := "✓ manager agent — \"m\" is running (container running)\n" +
		"✗ manager image — was created on evil:latest\n" +
		"    fix: run `lever up --fresh`�✓ fake row\n"
	if got != want {
		t.Fatalf("output:\n%q\nwant:\n%q", got, want)
	}
}

// TestPrintDoctorReportWarningRows: a warning row (warnResult) prints with
// "!" and its fix, and does not count as a failure — doctor's exit status is
// for faults, and a warning is a setting the operator chose.
func TestPrintDoctorReportWarningRows(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	checks := []checkResult{
		{"broker running", true, "pid 1", ""},
		warnResult("remote exposure", "remote.bind is 10.0.0.5", "firewall it"),
	}
	if failed := printDoctorReport(cmd, checks); failed != 0 {
		t.Fatalf("failed = %d, want 0: a warning is not a failure", failed)
	}
	want := "✓ broker running — pid 1\n" +
		"! remote exposure — remote.bind is 10.0.0.5\n" +
		"    fix: firewall it\n"
	if got := out.String(); got != want {
		t.Fatalf("output:\n%q\nwant:\n%q", got, want)
	}
}
