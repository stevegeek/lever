package cli

import "testing"

func TestUpDecision(t *testing.T) {
	cases := []struct {
		phase string // "" = absent
		fresh bool
		want  string // "apply" | "resume" | "none" | "restart"
	}{
		{"", false, "apply"},
		{"suspended", false, "resume"},
		{"running", false, "none"},
		{"running", true, "restart"},
		{"suspended", true, "restart"},
		{"stopped", false, "apply"},
	}
	for _, c := range cases {
		if got := upDecision(c.phase, c.fresh); got != c.want {
			t.Errorf("upDecision(%q,%v)=%q want %q", c.phase, c.fresh, got, c.want)
		}
	}
}
