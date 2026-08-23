package state

import (
	"testing"

	"github.com/stevegeek/lever/internal/skills"
)

func TestHashJSON(t *testing.T) {
	type v struct{ A int }
	first, again := HashJSON(v{1}), HashJSON(v{1})
	if first != again {
		t.Fatalf("HashJSON must be deterministic: %q != %q", first, again)
	}
	if other := HashJSON(v{2}); first == other {
		t.Fatalf("HashJSON must be sensitive to the value: both %q", first)
	}
	if HashJSON(v{1}) != skills.Hash([]byte(`{"A":1}`)) {
		t.Fatal("HashJSON must be skills.Hash over the JSON encoding")
	}
	if HashJSON(make(chan int)) != "" {
		t.Fatal("an unmarshalable value must hash to \"\" (guaranteed mismatch)")
	}
}
