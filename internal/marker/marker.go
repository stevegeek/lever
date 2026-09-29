// Package marker holds what lever treats as its own message markers, for the
// two places that must keep other writers from forging one: the broker
// (worker and manager bodies) and the remote proxy (a contact's chat).
package marker

import (
	"regexp"
	"strings"
)

// like matches anything a reader could take for a lever marker or for a
// scion envelope delimiter: "[lever:" with any case and spacing (fullwidth
// and white square brackets too), and the BEGIN/END SCION MESSAGE lines.
var like = regexp.MustCompile(`(?i)[\[［⟦〚][\s\p{Cf}]*lever[\s\p{Cf}]*[:：]|-{3}[\s\p{Cf}]*(begin|end)[\s\p{Cf}]+scion[\s\p{Cf}]+message[\s\p{Cf}]*-{3}`)

// Contains reports whether s holds a marker-like sequence anywhere.
func Contains(s string) bool { return like.MatchString(s) }

// Neutralise rewrites every marker-like sequence in s, so the text cannot
// claim to be a lever marker or a second envelope. The text stays readable.
func Neutralise(s string) string {
	return like.ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, "-") {
			return "(quoted scion delimiter)"
		}
		return "(quoted lever marker:"
	})
}

// openers are the characters a marker's first line starts with, in any
// script a reader could take for "[".
const openers = "[［⟦〚【〔｢「"

// FirstLineLooksLikeMarker reports whether the first non-blank line of s
// starts with a bracket: agents trust a marker on the first line only, so a
// writer who must never pass for lever may not start there with one, even
// in a spelling the pattern above does not catch ("[Iever:").
func FirstLineLooksLikeMarker(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimLeft(line, " \t\r\u200b\u200c\u200d\ufeff")
		if t == "" {
			continue
		}
		return strings.ContainsRune(openers, []rune(t)[0])
	}
	return false
}
