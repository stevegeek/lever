package config_test

import (
	"slices"
	"testing"

	"github.com/stevegeek/lever/internal/backend/registry"
	"github.com/stevegeek/lever/internal/config"
)

// TestKnownBackendsMatchCandidates pins config's backend list to
// registry.Candidates(), so neither can gain or lose a backend alone.
func TestKnownBackendsMatchCandidates(t *testing.T) {
	var want []string
	for _, c := range registry.Candidates() {
		want = append(want, c.Name)
	}
	if !slices.Equal(config.KnownBackends, want) {
		t.Fatalf("config.KnownBackends = %v, registry.Candidates() = %v", config.KnownBackends, want)
	}
	if got := config.BackendNames(); !slices.Equal(got, registry.Names()) {
		t.Fatalf("config.BackendNames() = %v, registry.Names() = %v", got, registry.Names())
	}
}
