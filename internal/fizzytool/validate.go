// Package fizzytool is the host side of lever-tool-fizzy: a narrow set of
// Fizzy operations on ONE board, run through the official fizzy CLI with the
// token in its environment. The agent never holds the Fizzy token, and every
// card the tool reads or writes is checked to be on the configured board.
package fizzytool

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	numberRE = regexp.MustCompile(`^[0-9]{1,7}$`)
	columnRE = regexp.MustCompile(`^[a-z0-9]{10,40}$`)
	pseudo   = map[string]bool{"not-now": true, "maybe": true, "done": true}
)

func ValidNumber(s string) error {
	if !numberRE.MatchString(s) {
		return fmt.Errorf("card number %q invalid (digits only)", s)
	}
	return nil
}

// ValidColumn accepts a column id; allowPseudo also admits the list filters
// not-now, maybe and done (never for a move).
func ValidColumn(s string, allowPseudo bool) error {
	if columnRE.MatchString(s) || (allowPseudo && pseudo[s]) {
		return nil
	}
	return fmt.Errorf("column %q invalid", s)
}

func noControl(s string, allowNewline bool) bool {
	for _, r := range s {
		if r == '\n' && allowNewline {
			continue
		}
		if r == '\t' && allowNewline {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return utf8.ValidString(s)
}

func ValidSearch(s string) error {
	if len(s) > 200 || !noControl(s, false) {
		return fmt.Errorf("search must be at most 200 characters with no control characters")
	}
	return nil
}

func ValidTitle(s string) error {
	if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 200 || !noControl(s, false) {
		return fmt.Errorf("title must be 1-200 characters on one line")
	}
	return nil
}

// attachmentMarkers are the Action Text markup a body could use to make an
// attachment: the CLI turns markdown into HTML but passes raw HTML through,
// and the server turns an <action-text-attachment sgid=...> (or a Trix
// <figure data-trix-attachment=...>) into a real attachment — a mention
// notifies an account user as the token owner, and a seen sgid re-embeds
// an attachment. Other raw HTML gives nothing markdown does not, so it is
// left alone.
var attachmentMarkers = []string{"action-text-attachment", "trix-attachment"}

func ValidText(s string) error {
	if utf8.RuneCountInString(s) > 20000 || !noControl(s, true) {
		return fmt.Errorf("text must be at most 20000 characters with no control characters other than newline and tab")
	}
	low := strings.ToLower(s)
	for _, m := range attachmentMarkers {
		if strings.Contains(low, m) {
			return fmt.Errorf("text must not contain %q: attachment and mention markup is refused (write plain markdown)", m)
		}
	}
	return nil
}
