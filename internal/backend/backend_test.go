package backend

import "testing"

func TestProfileSummary(t *testing.T) {
	p := Profile{Name: "orbstack", SeparateKernel: false, VersionFragile: true}
	if got := p.Summary(); got == "" {
		t.Fatal("Summary should be non-empty")
	}
}
