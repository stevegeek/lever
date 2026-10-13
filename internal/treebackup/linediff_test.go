package treebackup

import (
	"bytes"
	"fmt"
	"go/build"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// unified renders hunks the way Go's internal/diff does, for comparison
// with its test data.
func unified(hs []hunk) string {
	var b strings.Builder
	for _, h := range hs {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		for _, l := range h.Lines {
			b.WriteByte(l.Op)
			b.WriteString(l.Text)
			if !strings.HasSuffix(l.Text, "\n") {
				b.WriteString("\n\\ No newline at end of file\n")
			}
		}
	}
	return b.String()
}

func TestLineDiffMatchesGoInternalDiff(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(build.Default.GOROOT, "src", "internal", "diff", "testdata", "*.txt"))
	if len(files) == 0 {
		t.Skip("Go source test data not available")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		parts := map[string]string{}
		var name string
		for _, line := range strings.SplitAfter(string(data), "\n") {
			if strings.HasPrefix(line, "-- ") && strings.HasSuffix(line, " --\n") {
				name = strings.TrimSuffix(strings.TrimPrefix(line, "-- "), " --\n")
				continue
			}
			if name != "" {
				parts[name] += line
			}
		}
		// The test data writes a trailing space as "$" and a missing final
		// newline as "^D".
		clean := func(s string) string {
			return strings.TrimSuffix(strings.ReplaceAll(s, "$\n", "\n"), "^D\n")
		}
		fix := func(s string) []byte { return []byte(clean(s)) }
		want := clean(parts["diff"])
		if i := strings.Index(want, "@@"); i >= 0 {
			want = want[i:]
		} else {
			want = ""
		}
		got := unified(lineDiff(fix(parts["old"]), fix(parts["new"])))
		if got != want {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", filepath.Base(f), got, want)
		}
	}
}

func TestLineDiffBasic(t *testing.T) {
	got := unified(lineDiff([]byte("a\nb\nc\nd\ne\nf\ng\n"), []byte("w\na\nb\nx\ny\nz\ne\n")))
	want := "@@ -1,7 +1,7 @@\n+w\n a\n b\n-c\n-d\n+x\n+y\n+z\n e\n-f\n-g\n"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if lineDiff([]byte("same\n"), []byte("same\n")) != nil {
		t.Fatal("equal inputs gave hunks")
	}
}

func TestLineDiffPathologicalInputsAreFast(t *testing.T) {
	const n = 100_000
	r := rand.New(rand.NewPCG(3, 4))
	var distinct, same, shuffled bytes.Buffer
	perm := r.Perm(n)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&distinct, "%d\n", i)
		fmt.Fprintf(&shuffled, "%d\n", perm[i])
		same.WriteString("a\n")
	}
	pairs := [][2][]byte{
		{distinct.Bytes(), shuffled.Bytes()},
		{same.Bytes(), shuffled.Bytes()},
		{same.Bytes(), bytes.Repeat([]byte("b\n"), n)},
		{same.Bytes(), append(bytes.Repeat([]byte("a\n"), n-1), "b\n"...)},
	}
	for i, p := range pairs {
		start := time.Now()
		hs := lineDiff(p[0], p[1])
		var out bytes.Buffer
		ow := &outWriter{w: &out}
		writeSection(ow, []byte("f"), p[0], p[1])
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("pair %d took %v", i, d)
		}
		if len(hs) == 0 || out.Len() == 0 {
			t.Errorf("pair %d: no diff", i)
		}
	}
}
