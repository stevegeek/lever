package servicehost

import (
	"strings"
	"testing"
)

func TestFoldedKeyCollision(t *testing.T) {
	for body, want := range map[string]bool{
		`{"method":"tools/call","params":{"name":"a"}}`: false,
		`{"a":1,"a":2}`: true,
		`{"params":{"name":"echo"},"paramſ":{"name":"forbidden"}}`: true,
		`{"params":{"name":"echo","Name":"x"}}`:                    true,
		`{"params":{"arguments":{"table":"A","TABLE":"B"}}}`:       true,
		`{"x":[{"k":1},{"K":2}]}`:                                  false,
		`{"x":[{"k":1,"K":2}]}`:                                    true,
		`{"x":{"y":{"z":{"kelvin":1,"Kelvin":2}}}}`:                true,
		`{"a":[1,{"b":[]},"c"],"d":{"e":null}}`:                    false,
		`{"a":`:                                                    true,
		`not json`:                                                 true,
		`{"a":1}{"b":2}`:                                           false,
		strings.Repeat(`[`, 300) + strings.Repeat(`]`, 300):        true,
		strings.Repeat(`[`, 200) + strings.Repeat(`]`, 200):        false,
	} {
		if got := FoldedKeyCollision([]byte(body)); got != want {
			t.Errorf("%s: %v, want %v", body, got, want)
		}
	}
}
