package host

import (
	"testing"

	"github.com/stevegeek/lever/internal/wire"
)

// TestFirstLine: scion's error carries its usage dump after the first line,
// which must never reach the terminal.
func TestFirstLine(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"multi-line input keeps only the first line",
			"Error: Hub at http://127.0.0.1:8080 is not responding: dial tcp: connect: connection refused\n\nUsage:\n  scion list [flags]\n\nFlags:\n  -h, --help   help for list\n",
			"Error: Hub at http://127.0.0.1:8080 is not responding: dial tcp: connect: connection refused",
		},
		{"single line input is unchanged", "no git origin remote found for this project.", "no git origin remote found for this project."},
		{"empty input stays empty", "", ""},
		{"leading/trailing whitespace on the first line is trimmed", "  Error: project not found (status: 404)  \nUsage:\n  scion list\n", "Error: project not found (status: 404)"},
		// scion colours its errors; a compromised jail can emit anything.
		{"ANSI colour on the first line is stripped", "\x1b[1;31mError:\x1b[0m hub down\nUsage:\n", "Error: hub down"},
		{"OSC title and CR on the first line are neutralised", "ok\x1b]0;pwned\x07\rrow\nrest", "ok\ufffdrow"},
	}
	for _, c := range cases {
		if got := firstLine(c.input); got != c.want {
			t.Errorf("%s: firstLine(%q)=%q want %q", c.name, c.input, got, c.want)
		}
	}
}

// brokerReusable: only a broker whose /epoch identity matches this binary and
// broker config is kept; empty fields always mismatch.
func TestBrokerReusable(t *testing.T) {
	got := wire.EpochResponse{Version: "v", ConfigHash: "h"}
	if !brokerReusable(got, "v", "h") {
		t.Fatal("matching identity must be reusable")
	}
	if brokerReusable(got, "v2", "h") {
		t.Fatal("binary version drift must force a restart")
	}
	if brokerReusable(got, "v", "h2") {
		t.Fatal("config drift must force a restart")
	}
	if brokerReusable(wire.EpochResponse{}, "v", "h") {
		t.Fatal("an old broker (no identity fields) must force a restart")
	}
}
