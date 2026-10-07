package fizzytool

import (
	"strings"
	"testing"
)

func TestValidators(t *testing.T) {
	ok := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s should be valid: %v", what, err)
		}
	}
	bad := func(err error, what string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s should be refused", what)
		}
	}
	ok(ValidNumber("17"), "17")
	for _, n := range []string{"", "-1", "1a", "12345678", "--help"} {
		bad(ValidNumber(n), n)
	}
	ok(ValidColumn("03gyvmty0g3eep8905yekgq4m", false), "column id")
	ok(ValidColumn("done", true), "pseudo done (list)")
	bad(ValidColumn("done", false), "pseudo done (move)")
	bad(ValidColumn("-x", false), "-x")
	bad(ValidColumn("ABCDEFGHIJKL", false), "uppercase")
	ok(ValidSearch("nested virt"), "search")
	bad(ValidSearch(strings.Repeat("a", 201)), "long search")
	bad(ValidSearch("a\x00b"), "control char")
	ok(ValidTitle("Fix the doctor row"), "title")
	bad(ValidTitle(""), "empty title")
	bad(ValidTitle("a\nb"), "newline title")
	bad(ValidTitle(strings.Repeat("a", 201)), "long title")
	ok(ValidText("line 1\nline 2"), "body")
	bad(ValidText(strings.Repeat("a", 20001)), "long body")
	bad(ValidText("a\x00b"), "NUL body")
	for _, b := range []string{
		`hi <action-text-attachment sgid="BAh7CEkiCGdpZAY"></action-text-attachment>`,
		`<ACTION-TEXT-ATTACHMENT sgid="x" content-type="application/vnd.actiontext.mention">`,
		`<figure data-trix-attachment='{"sgid":"x"}'></figure>`,
	} {
		bad(ValidText(b), b)
	}
	ok(ValidText("see `Vec<T>` and <b>bold</b>, a < b"), "other markup")
}
