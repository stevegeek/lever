package voice

// Text for a lexicon-only engine. sherpa-onnx 2.x turns a word into sounds
// only through its lexicons and skips any word in none of them (sherpa.go,
// assumption 4): numbers, most acronyms, tool names. So lever rewrites the
// text first: numbers become words, a word the lexicons lack is split at
// its case changes and spelled letter by letter where a part is still
// unknown, and symbols that are not punctuation become spaces. Every
// lexicon lever pins has the number words below and each single letter as
// the letter's name ("b" -> "bee"), which is what spelling relies on.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// Lexicon is the set of words, lower case, that the engine's lexicons hold
// for one voice.
type Lexicon map[string]struct{}

func (l Lexicon) has(w string) bool {
	_, ok := l[strings.ToLower(w)]
	return ok
}

// maxLexiconBytes bounds one lexicon file lever reads (the pinned ones are
// a few MB); its sha256 was checked with the model's files.
const maxLexiconBytes = 64 << 20

// LoadLexicon reads the words of lexicon files in sherpa-onnx's formats: a
// line is "word phones..." or "word || phones...", and a line starting with
// "#" is a comment. An entry of more than one word is not a word lever
// looks up, so it is left out.
func LoadLexicon(paths ...string) (Lexicon, error) {
	lex := Lexicon{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		err = readLexicon(io.LimitReader(f, maxLexiconBytes+1), lex)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return lex, nil
}

func readLexicon(r io.Reader, lex Lexicon) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var n int64
	for sc.Scan() {
		line := sc.Text()
		n += int64(len(line)) + 1
		if n > maxLexiconBytes {
			return fmt.Errorf("more than %d bytes", maxLexiconBytes)
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, ok := strings.Cut(line, " || ")
		if !ok {
			key, _, _ = strings.Cut(line, " ")
		}
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		lex[strings.ToLower(key)] = struct{}{}
	}
	return sc.Err()
}

// SpeakableText rewrites text (which passed SpeechInput) so that every word
// in it is in lex: what the engine would skip is spoken instead. It keeps
// sentence punctuation for the engine's pauses. The result starts with a
// letter (assumption 5); "" means nothing speakable is left.
func SpeakableText(text string, lex Lexicon) string {
	var out []string
	add := func(w ...string) { out = append(out, w...) }
	punct := func(r rune) {
		// Punctuation goes with the word before it, as written.
		if len(out) > 0 {
			out[len(out)-1] += string(r)
		}
	}
	rs := []rune(foldLatin(strings.ReplaceAll(text, "’", "'")))
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case isWordRune(r):
			j := i
			for j < len(rs) && (isWordRune(rs[j]) || rs[j] == '\'' && j+1 < len(rs) && isWordRune(rs[j+1]) && j > i) {
				j++
			}
			add(speakWord(string(rs[i:j]), lex)...)
			i = j
		case (r == '-' || r == '−') && i+1 < len(rs) && isDigit(rs[i+1]) && (i == 0 || !isWordRune(rs[i-1]) && !isDigit(rs[i-1])):
			// A sign, not a range's or a word's hyphen: "-5", "(−3)".
			add("minus")
			i++
		case currencyWord(r) != "" && i+1 < len(rs) && isDigit(rs[i+1]):
			j, words := readNumber(rs, i+1)
			add(words...)
			if len(words) == 1 && words[0] == "one" {
				add(currencyWord(r))
			} else {
				add(currencyWord(r) + "s")
			}
			i = j
		case r >= '0' && r <= '9':
			j, words := readNumber(rs, i)
			// An ordinal suffix: 1st, 22nd, 3rd, 4th.
			if k := j + 2; k <= len(rs) && (k == len(rs) || !isWordRune(rs[k])) && ordinalSuffix(string(rs[j:k])) && len(words) > 0 {
				words[len(words)-1] = ordinal(words[len(words)-1])
				j = k
			}
			add(words...)
			i = j
		case r == '%':
			add("percent")
			i++
		case r == '&':
			add("and")
			i++
		case r == '+':
			add("plus")
			i++
		case r == '@':
			add("at")
			i++
		case strings.ContainsRune(".,!?;:", r):
			punct(r)
			i++
		default:
			// Any other symbol is a space: a path's "/", a branch's "-",
			// quotes, brackets.
			i++
		}
	}
	s := strings.Join(out, " ")
	// Nothing but punctuation is left: nothing to speak.
	if !strings.ContainsFunc(s, unicode.IsLetter) {
		return ""
	}
	return strings.TrimLeftFunc(s, func(r rune) bool { return !unicode.IsLetter(r) })
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.Is(unicode.Mn, r) }

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// currencyWord is the unit a currency sign before a number is read as.
func currencyWord(r rune) string {
	switch r {
	case '$':
		return "dollar"
	case '£':
		return "pound"
	case '€':
		return "euro"
	}
	return ""
}

// latinFold is the plain letters of the accented Latin letters (Latin-1
// and Latin Extended-A): the pinned lexicons are English and hold no
// accented words, so "café" is read as "cafe" and spelled with its "e".
var latinFold = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a", 'ă': "a", 'ą': "a",
	'À': "A", 'Á': "A", 'Â': "A", 'Ã': "A", 'Ä': "A", 'Å': "A", 'Ā': "A", 'Ă': "A", 'Ą': "A",
	'æ': "ae", 'Æ': "AE", 'ç': "c", 'Ç': "C", 'ć': "c", 'Ć': "C", 'č': "c", 'Č': "C", 'ĉ': "c", 'ċ': "c",
	'ď': "d", 'Ď': "D", 'đ': "d", 'Đ': "D", 'ð': "d", 'Ð': "D",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e", 'ĕ': "e", 'ė': "e", 'ę': "e", 'ě': "e",
	'È': "E", 'É': "E", 'Ê': "E", 'Ë': "E", 'Ē': "E", 'Ĕ': "E", 'Ė': "E", 'Ę': "E", 'Ě': "E",
	'ĝ': "g", 'ğ': "g", 'ġ': "g", 'ģ': "g", 'Ğ': "G", 'ĥ': "h", 'ħ': "h",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ĩ': "i", 'ī': "i", 'ĭ': "i", 'į': "i", 'ı': "i",
	'Ì': "I", 'Í': "I", 'Î': "I", 'Ï': "I", 'Ī': "I", 'İ': "I", 'ĵ': "j", 'ķ': "k",
	'ĺ': "l", 'ļ': "l", 'ľ': "l", 'ł': "l", 'Ł': "L", 'ñ': "n", 'Ñ': "N", 'ń': "n", 'ņ': "n", 'ň': "n", 'Ń': "N",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o", 'ŏ': "o", 'ő': "o",
	'Ò': "O", 'Ó': "O", 'Ô': "O", 'Õ': "O", 'Ö': "O", 'Ø': "O", 'Ō': "O", 'Ő': "O", 'œ': "oe", 'Œ': "OE",
	'ŕ': "r", 'ř': "r", 'Ř': "R", 'ś': "s", 'ş': "s", 'š': "s", 'ŝ': "s", 'Ś': "S", 'Ş': "S", 'Š': "S", 'ß': "ss",
	'ţ': "t", 'ť': "t", 'ŧ': "t", 'Ť': "T", 'þ': "th", 'Þ': "TH",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ũ': "u", 'ū': "u", 'ŭ': "u", 'ů': "u", 'ű': "u", 'ų': "u",
	'Ù': "U", 'Ú': "U", 'Û': "U", 'Ü': "U", 'Ū': "U", 'Ů': "U", 'Ű': "U",
	'ŵ': "w", 'ý': "y", 'ÿ': "y", 'ŷ': "y", 'Ý': "Y", 'Ÿ': "Y", 'ź': "z", 'ż': "z", 'ž': "z", 'Ź': "Z", 'Ż': "Z", 'Ž': "Z",
}

// foldLatin replaces accented Latin letters with plain ones and drops the
// combining marks after a letter (a decomposed "é"). Other scripts are left
// as they are.
func foldLatin(s string) string {
	var b strings.Builder
	var prev rune
	for _, r := range s {
		f, folded := latinFold[r]
		switch {
		case folded:
			b.WriteString(f)
		case unicode.Is(unicode.Mn, r) && prev < 0x250 && unicode.IsLetter(prev):
			// A mark on a Latin letter is dropped.
		default:
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

// speakWord returns w as words the lexicon holds: w itself; else its parts
// at case changes ("GitHub" -> "Git Hub", "HTTPServer" -> "HTTP Server"),
// each kept, or its singular kept ("configs" -> "config" when only that is
// known), or spelled a letter at a time. Accents were folded before
// (foldLatin), so a part with no letter of a to z is one of another script
// ("привет"), which is dropped.
func speakWord(w string, lex Lexicon) []string {
	if lex.has(w) {
		return []string{w}
	}
	if strings.ContainsRune(w, '\'') {
		var out []string
		for _, p := range strings.Split(w, "'") {
			if p != "" {
				out = append(out, speakWord(p, lex)...)
			}
		}
		return out
	}
	var out []string
	for _, p := range caseParts(w) {
		switch {
		case lex.has(p):
			out = append(out, p)
		case len(p) > 3 && (p[len(p)-1] == 's' || p[len(p)-1] == 'S') && lex.has(p[:len(p)-1]):
			out = append(out, p[:len(p)-1])
		default:
			out = append(out, spell(p, lex)...)
		}
	}
	return out
}

// caseParts splits w where its case changes: before an upper case letter
// after a lower case one, and before the last of a run of upper case
// letters followed by a lower case one. A lone lower case "s" after upper
// case letters (a plural acronym, "PRs") is its own part.
func caseParts(w string) []string {
	rs := []rune(w)
	var parts []string
	start := 0
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		split := unicode.IsLower(prev) && unicode.IsUpper(cur)
		if unicode.IsUpper(prev) && unicode.IsLower(cur) && i-start >= 2 {
			// "HTTPServer": the S starts the next part; "PRs": the s does.
			if cur == 's' && (i+1 == len(rs) || !unicode.IsLetter(rs[i+1])) {
				parts = append(parts, string(rs[start:i]))
				start = i
				continue
			}
			parts = append(parts, string(rs[start:i-1]))
			start = i - 1
			continue
		}
		if split {
			parts = append(parts, string(rs[start:i]))
			start = i
		}
	}
	return append(parts, string(rs[start:]))
}

// spell is w's letters a to z, lower case, one word each (every pinned
// lexicon reads a single letter as its name); other letters are dropped.
func spell(w string, lex Lexicon) []string {
	var out []string
	for _, r := range strings.ToLower(w) {
		if r >= 'a' && r <= 'z' && lex.has(string(r)) {
			out = append(out, string(r))
		}
	}
	return out
}

// readNumber reads the number at rs[i]: digits, with "," thousands groups
// (1,000) and "." decimals (3.5, and a version's 1.0.2). It returns where it
// ends and its words.
func readNumber(rs []rune, i int) (int, []string) {
	digits := func(j int) int {
		for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
			j++
		}
		return j
	}
	j := digits(i)
	whole := string(rs[i:j])
	// Thousands groups: 1-3 digits, then ",ddd" groups not followed by a
	// digit.
	if j-i <= 3 {
		for j+4 <= len(rs) && rs[j] == ',' && digits(j+1) == j+4 {
			whole += string(rs[j+1 : j+4])
			j += 4
		}
	}
	words := numberWords(whole)
	// Decimals and version parts: ".d+" read digit by digit.
	for j+1 < len(rs) && rs[j] == '.' && rs[j+1] >= '0' && rs[j+1] <= '9' {
		k := digits(j + 1)
		words = append(words, "point")
		for _, d := range rs[j+1 : k] {
			words = append(words, smallNumbers[d-'0'])
		}
		j = k
	}
	return j, words
}

var smallNumbers = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine",
	"ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}

var tens = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}

// numberWords reads a string of digits as a cardinal number up to the
// billions; a longer one, or one with a leading zero (007, a PIN), is read
// digit by digit.
func numberWords(d string) []string {
	if len(d) > 12 || len(d) > 1 && d[0] == '0' {
		out := make([]string, 0, len(d))
		for _, c := range d {
			out = append(out, smallNumbers[c-'0'])
		}
		return out
	}
	var n int64
	for _, c := range d {
		n = n*10 + int64(c-'0')
	}
	if n == 0 {
		return []string{"zero"}
	}
	var out []string
	for _, s := range []struct {
		v    int64
		name string
	}{{1e9, "billion"}, {1e6, "million"}, {1e3, "thousand"}} {
		if n >= s.v {
			out = append(out, below1000(int(n/s.v))...)
			out = append(out, s.name)
			n %= s.v
		}
	}
	return append(out, below1000(int(n))...)
}

func below1000(n int) []string {
	var out []string
	if n >= 100 {
		out = append(out, smallNumbers[n/100], "hundred")
		n %= 100
	}
	switch {
	case n == 0:
	case n < 20:
		out = append(out, smallNumbers[n])
	default:
		out = append(out, tens[n/10])
		if n%10 != 0 {
			out = append(out, smallNumbers[n%10])
		}
	}
	return out
}

func ordinalSuffix(s string) bool {
	switch strings.ToLower(s) {
	case "st", "nd", "rd", "th":
		return true
	}
	return false
}

// ordinal turns the last word of a number into its ordinal.
func ordinal(w string) string {
	switch w {
	case "one":
		return "first"
	case "two":
		return "second"
	case "three":
		return "third"
	case "five":
		return "fifth"
	case "eight":
		return "eighth"
	case "nine":
		return "ninth"
	case "twelve":
		return "twelfth"
	}
	if strings.HasSuffix(w, "y") {
		return strings.TrimSuffix(w, "y") + "ieth"
	}
	return w + "th"
}
