package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readFileTail(p, 4)
	if err != nil || string(got) != "6789" {
		t.Fatalf("tail(4) = %q, %v; want 6789", got, err)
	}
	got, err = readFileTail(p, 100)
	if err != nil || string(got) != "0123456789" {
		t.Fatalf("tail(100) = %q, %v; want the whole file", got, err)
	}
	if _, err := readFileTail(filepath.Join(t.TempDir(), "missing"), 4); err == nil {
		t.Fatal("a missing file must return an error")
	}
}

func TestLastLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"\n\n", ""},
		{"one", "one"},
		{"one\ntwo\n", "two"},
		{"one\ntwo\n  \n\n", "two"},
		{"one\n  two  \n", "  two  "},
	} {
		if got := lastLine(tc.in); got != tc.want {
			t.Errorf("lastLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLastFileLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "log")
	body := strings.Repeat("early line\n", 100) + "  final: exit 1  \n\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastFileLine(p, 64); got != "final: exit 1" {
		t.Fatalf("lastFileLine = %q", got)
	}
	if got := lastFileLine(filepath.Join(t.TempDir(), "missing"), 64); got != "" {
		t.Fatalf("missing file must give \"\", got %q", got)
	}
}
