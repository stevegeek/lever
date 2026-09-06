package host

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestSanitizeTerminal pins the choke point for guest-supplied strings
// (scion stderr, hub slugs, container status, the record's image) before
// they reach the operator's terminal: escape sequences are stripped whole,
// control bytes are replaced, and ordinary UTF-8 passes unchanged.
func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain ASCII unchanged", "manager is running (container running)", "manager is running (container running)"},
		{"plain UTF-8 unchanged", "✓ scaffold — current (lever-operator + workers) — café", "✓ scaffold — current (lever-operator + workers) — café"},
		{"empty", "", ""},
		{"OSC title (BEL-terminated) stripped whole", "before\x1b]0;doctor: all ok\x07after", "beforeafter"},
		{"OSC 52 clipboard (ST-terminated) stripped whole", "x\x1b]52;c;aGVsbG8=\x1b\\y", "xy"},
		{"CSI clear screen stripped", "\x1b[2Jrunning", "running"},
		{"CSI colour with params stripped", "\x1b[1;31mError\x1b[0m: nope", "Error: nope"},
		{"CSI cursor-up row spoof stripped", "ok\x1b[3A\x1b[2K✗ broker — dead", "ok✗ broker — dead"},
		{"two-byte ESC sequence stripped", "\x1bcreset", "reset"},
		{"nF charset sequence stripped", "\x1b(Btext", "text"},
		{"DCS stripped to ST", "a\x1bPq...\x1b\\b", "ab"},
		{"C1 CSI (U+009B) stripped", "a\u009b2Jb", "ab"},
		{"C1 OSC (U+009D) stripped to C1 ST", "a\u009d0;t\u009cb", "ab"},
		{"lone C1 byte replaced", "a\u0080b", "a\ufffdb"},
		{"C0 bytes replaced, tab and newline included", "a\tb\nc\rd\x00e", "a�b�c�d�e"},
		{"DEL replaced", "a\x7fb", "a�b"},
		{"bare trailing ESC dropped", "abc\x1b", "abc"},
		{"unterminated CSI dropped to end", "abc\x1b[1;2", "abc"},
		{"unterminated OSC dropped to end", "abc\x1b]0;title", "abc"},
		{"invalid UTF-8 byte replaced", "a\x9bb", "a�b"},
	}
	for _, c := range cases {
		if got := sanitizeTerminal(c.in); got != c.want {
			t.Errorf("%s: sanitizeTerminal(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

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
