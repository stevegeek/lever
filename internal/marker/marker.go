// Package marker holds what lever treats as its own message markers, for the
// two places that must keep other writers from forging one: the broker
// (worker and manager bodies) and the remote proxy (a contact's chat).
package marker

import (
	"regexp"
	"strings"
	"unicode"
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

// leverWord matches "lever" followed by a colon or a space, in any case and
// with common look-alikes for its letters (Cyrillic е, dotless ı, l/I/1/|),
// the way a forged marker would spell it.
var leverWord = regexp.MustCompile(`(?i)(?:^|[^\p{L}])[lI1|ǀ][\p{Cf}]*[eеéè][\p{Cf}]*[vѵν][\p{Cf}]*[eеéè][\p{Cf}]*[rгʀ][\p{Cf}]*[\s:：]`)

// FirstLineLooksLikeMarker reports whether the first non-blank line of s
// could pass for a lever marker: it starts with anything but a letter or a
// digit, or it holds the word "lever" followed by a colon or a space. Agents
// trust a marker on the first line only, so a writer who must never pass for
// lever may not write one there in any spelling. Leading space is trimmed
// the way the hub trims a message (unicode.IsSpace), plus format characters.
func FirstLineLooksLikeMarker(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimLeftFunc(line, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) })
		if t == "" {
			continue
		}
		r := []rune(t)[0]
		return !(unicode.IsLetter(r) || unicode.IsDigit(r)) || leverWord.MatchString(t)
	}
	return false
}
