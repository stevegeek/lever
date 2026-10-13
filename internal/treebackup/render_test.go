package treebackup

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestQ(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"a/b.txt", "a/b.txt"},
		{"x\xff", `"x\xff"`},
		{"x\xfe", `"x\xfe"`},
		{"a b", `"a b"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{"a\tb\nc\rd", `"a\tb\nc\rd"`},
		{"\x1b[31m", `"\x1b[31m"`},
		{"\x7f", `"\x7f"`},
		{"caf\u00E9", `"caf\xc3\xa9"`},   // NFC
		{"cafe\u0301", `"cafe\xcc\x81"`}, // NFD
		{"\u0430", `"\xd0\xb0"`},         // Cyrillic a
		{"a\u202Eb", `"a\xe2\x80\xaeb"`},
		{"", `""`},
	}
	for _, c := range cases {
		if got := Q([]byte(c.in)); got != c.want {
			t.Errorf("Q(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestQDistinguishesInvalidUTF8Names(t *testing.T) {
	a, b := Q([]byte("x\xff")), Q([]byte("x\xfe"))
	if a == b {
		t.Fatalf("both render as %s", a)
	}
	for _, s := range []string{a, b} {
		p, err := ParsePathArg("q:" + s)
		if err != nil || Q(p) != s {
			t.Fatalf("q:%s round trip: %q, %v", s, p, err)
		}
	}
}

func TestQRoundTripRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	seen := map[string]string{}
	alphabet := []byte("ab\"\\ \t\n\r\x00\x01\x7f\x80\xff/:q.x")
	for i := 0; i < 20000; i++ {
		b := make([]byte, r.IntN(8))
		for j := range b {
			if r.IntN(4) == 0 {
				b[j] = byte(r.IntN(256))
			} else {
				b[j] = alphabet[r.IntN(len(alphabet))]
			}
		}
		q := Q(b)
		got, err := ParseQ(q)
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("ParseQ(Q(%q)) = %q, %v", b, got, err)
		}
		if prev, ok := seen[q]; ok && prev != string(b) {
			t.Fatalf("%q and %q both render as %s", prev, b, q)
		}
		seen[q] = string(b)
	}
}

func FuzzQRoundTrip(f *testing.F) {
	for _, s := range []string{"", "a", "x\xff", `"a"`, `\`, "a b", "\x00"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		q := Q(b)
		got, err := ParseQ(q)
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("ParseQ(Q(%q)) = %q, %v", b, got, err)
		}
		if !strings.HasPrefix(q, `"`) && q != string(b) {
			t.Fatalf("Q(%q) = %s mixes the quoted and plain forms", b, q)
		}
	})
}

func TestParseQRejectsWhatQNeverPrints(t *testing.T) {
	for _, s := range []string{
		`"abc"`,     // plain paths are not quoted
		`"\x41"`,    // a printable byte is not escaped
		`"\xFF"`,    // uppercase hex
		`"\xf"`,     // short escape
		"\"a\x01\"", // raw control byte
		"\"a\xff\"", // raw high byte
		`"a`,        // no closing quote
		`"a\"`,      // escaped closing quote
		`"a"b"`,     // stray quote
		`"\q"`,      // unknown escape
		"a b",       // unquoted space
		`a\b`,       // unquoted backslash
		"a\xff",     // unquoted high byte
		"",
	} {
		if b, err := ParseQ(s); err == nil {
			t.Errorf("ParseQ(%q) = %q, want an error", s, b)
		}
	}
}

func TestParsePathArg(t *testing.T) {
	ok := map[string]string{
		"a/b":            "a/b",
		`q:"x\xff"`:      "x\xff",
		`q:"a b/c"`:      "a b/c",
		"q:docs/a.md":    "docs/a.md",
		"q:q:foo":        "q:foo",
		"with space":     "with space",
		"x\xff":          "x\xff",
		`q:"\xd0\xb0"`:   "\u0430",
		`q:"tab\there"`:  "tab\there",
		"weird\"name\\x": "weird\"name\\x",
	}
	for in, want := range ok {
		got, err := ParsePathArg(in)
		if err != nil || string(got) != want {
			t.Errorf("ParsePathArg(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/etc/passwd", "a/", "a//b", "./a", "a/./b", "a/..", "..", ".", "a\x00b",
		"q:", `q:""`, `q:"/a"`, `q:"a\x00"`, `q:"abc"`, `q:a b`, "q:a/", strings.Repeat("a", MaxPathCap+1)} {
		if got, err := ParsePathArg(in); err == nil {
			t.Errorf("ParsePathArg(%q) = %q, want an error", in, got)
		}
	}
}

func TestContentLineGolden(t *testing.T) {
	long := strings.Repeat("a", 1005)
	longMulti := strings.Repeat("a", 999) + "\u00E9" + "bcdef" // the two-byte rune straddles the 1000-byte cut
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "+ hello world"},
		{"tab is raw", "a\tb", "+ a\tb"},
		{"CR", "abc\r", `+\abc\x0d`},
		{"ESC", "\x1b[31mred\x1b[0m", `+\\x1b[31mred\x1b[0m`},
		{"fake diff header", "--- a/etc/passwd", "+ --- a/etc/passwd"},
		{"fake section header", "== M secret.txt", "+ == M secret.txt"},
		{"fake hunk header", "@@ -1,1 +1,1 @@", "+ @@ -1,1 +1,1 @@"},
		{"NFC", "caf\u00E9", "+ caf\u00E9"},
		{"NFD", "cafe\u0301", "+ cafe\u0301"},
		{"homoglyph", "p\u0430ypal", "+ p\u0430ypal"},
		{"tag characters", "a\U000E0041\U000E007Fb", `+\a\u{E0041}\u{E007F}b`},
		{"bidi override", "x\u202Ey", `+\x\u{202E}y`},
		{"bidi isolate", "\u2066x\u2069", `+\\u{2066}x\u{2069}`},
		{"zero width joiner", "a\u200Db", `+\a\u{200D}b`},
		{"BOM", "\uFEFFx", `+\\u{FEFF}x`},
		{"line separator", "a\u2028b", `+\a\u{2028}b`},
		{"paragraph separator", "a\u2029b", `+\a\u{2029}b`},
		{"variation selector", "\u2764\uFE0F", `+\` + "\u2764" + `\u{FE0F}`},
		{"variation selector supplement", "x\U000E0100", `+\x\u{E0100}`},
		{"C1", "a\u0085b", `+\a\u{0085}b`},
		{"DEL", "a\x7fb", `+\a\x7fb`},
		{"NUL", "a\x00b", `+\a\x00b`},
		{"private use", "a\uE000", `+\a\u{E000}`},
		{"unassigned", "a\u0378", `+\a\u{0378}`},
		{"backslash doubled when escaped", "a\\b\x1b", `+\a\\b\x1b`},
		{"backslash raw when not", `a\b`, `+ a\b`},
		{"invalid UTF-8", "a\xffb", `+\a\xffb`},
		{"forged cut marker", "x\u2026(+5 bytes)", "+ x\u2026(+5 bytes)"},
		{"forged escaped marker", `x\u{2026}(+5 bytes)` + "\x1b", `+\x\\u{2026}(+5 bytes)\x1b`},
		{"long line", long, `+\` + strings.Repeat("a", 1000) + `\u{2026}(+5 bytes)`},
		{"long line rune boundary", longMulti, `+\` + strings.Repeat("a", 999) + `\u{2026}(+7 bytes)`},
	}
	for _, c := range cases {
		got := contentLine('+', []byte(c.in))
		if got != c.want {
			t.Errorf("%s: contentLine(%q)\n got %s\nwant %s", c.name, c.in, got, c.want)
		}
		if got[1] != ' ' && got[1] != '\\' {
			t.Errorf("%s: second byte %q", c.name, got[1])
		}
	}
}

func TestContentLineMarkerCannotBeForged(t *testing.T) {
	raw := contentLine('+', []byte(strings.Repeat("a", 980)+"\u2026(+5 bytes)"))
	cut := contentLine('+', []byte(strings.Repeat("a", 1005)))
	if strings.HasPrefix(raw, `+\`) || !strings.HasPrefix(cut, `+\`) {
		t.Fatalf("raw %q\u2026 cut %q\u2026", raw[:3], cut[:3])
	}
	if strings.Contains(raw, `\u{2026}`) {
		t.Fatal("raw line holds the escaped marker")
	}
	// An escaped line that contains the marker text has its backslash
	// doubled, so the only single-backslash \u{2026} is lever's own.
	esc := contentLine('+', []byte(`\u{2026}(+5 bytes)`+"\x01"))
	if !strings.Contains(esc, `\\u{2026}`) {
		t.Fatalf("escaped line %q", esc)
	}
}
