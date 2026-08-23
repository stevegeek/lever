package state

import (
	"testing"

	"github.com/stevegeek/lever/internal/skills"
)

func TestHashJSON(t *testing.T) {
	type v struct{ A int }
	if HashJSON(v{1}) != HashJSON(v{1}) || HashJSON(v{1}) == HashJSON(v{2}) {
		t.Fatal("HashJSON must be deterministic and sensitive to the value")
	}
	if HashJSON(v{1}) != skills.Hash([]byte(`{"A":1}`)) {
		t.Fatal("HashJSON must be skills.Hash over the JSON encoding")
	}
	if HashJSON(make(chan int)) != "" {
		t.Fatal("an unmarshalable value must hash to \"\" (guaranteed mismatch)")
	}
}
