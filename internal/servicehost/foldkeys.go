package servicehost

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
)

// FoldedKeyCollision reports whether some JSON object in body, at any depth,
// has two keys that differ but are equal under Unicode case folding
// ("params" and "paramſ"). A checker that reads map keys exactly and a
// server that decodes into Go structs, which match keys case-insensitively,
// would read such a message differently, so it must be refused before it is
// checked. Invalid JSON reports true.
func FoldedKeyCollision(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var stack []*jsonFrame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return len(stack) != 0
		}
		if err != nil {
			return true
		}
		if n := len(stack); n > 0 && stack[n-1].keys != nil && stack[n-1].expectKey {
			top := stack[n-1]
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = valueDone(stack[:n-1])
				continue
			}
			k, ok := tok.(string)
			if !ok {
				return true
			}
			f := foldKey(k)
			if prev, seen := top.keys[f]; seen && prev != k {
				return true
			}
			top.keys[f] = k
			top.expectKey = false
			continue
		}
		switch d, _ := tok.(json.Delim); d {
		case '{':
			stack = append(stack, &jsonFrame{keys: map[string]string{}, expectKey: true})
		case '[':
			stack = append(stack, &jsonFrame{})
		case ']', '}':
			if len(stack) == 0 {
				return true
			}
			stack = valueDone(stack[:len(stack)-1])
		default:
			stack = valueDone(stack)
		}
	}
}

// jsonFrame is one open container: an object's folded keys, or nil keys
// for an array.
type jsonFrame struct {
	keys      map[string]string
	expectKey bool
}

// valueDone marks that the innermost object, if any, now waits for a key.
func valueDone(stack []*jsonFrame) []*jsonFrame {
	if n := len(stack); n > 0 && stack[n-1].keys != nil {
		stack[n-1].expectKey = true
	}
	return stack
}

// foldKey maps every rune to the smallest rune of its case-folding orbit,
// so two keys fold equal exactly when their foldKeys are equal.
func foldKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < m {
				m = f
			}
		}
		b.WriteRune(m)
	}
	return b.String()
}
