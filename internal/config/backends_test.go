package config_test

import (
	"slices"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/config"
)

// TestKnownBackendsMatchCandidates pins config's backend list to
// backend.Candidates, so neither can gain or lose a backend alone.
func TestKnownBackendsMatchCandidates(t *testing.T) {
	var want []string
	for _, c := range backend.Candidates {
		want = append(want, c.Name)
	}
	if !slices.Equal(config.KnownBackends, want) {
		t.Fatalf("config.KnownBackends = %v, backend.Candidates = %v", config.KnownBackends, want)
	}
	if got := config.BackendNames(); !slices.Equal(got, backend.Names()) {
		t.Fatalf("config.BackendNames() = %v, backend.Names() = %v", got, backend.Names())
	}
}
