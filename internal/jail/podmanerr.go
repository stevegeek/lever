package jail

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/termsafe"
)

// podman load writes its progress to stderr before it fails and the cause
// comes last, so an error keeps only the tail of the output.
const (
	podmanErrTailLines = 12   // lines kept from the end
	podmanErrLineMax   = 600  // bytes kept of one line: its head and its tail
	podmanErrTailMax   = 4096 // bytes kept in all; earlier lines go first
)

// podmanProgressPrefixes are the progress lines podman writes while it
// copies an image.
var podmanProgressPrefixes = []string{
	"Getting image source signatures",
	"Copying blob ",
	"Copying config ",
	"Writing manifest to image destination",
	"Storing signatures",
}

// StorageCorruptionHint is added to a podman error that names damage in the
// jail's container store.
const StorageCorruptionHint = "the jail's podman store looks damaged: run `lever doctor` (podman storage row) " +
	"and follow \"Recovering the jail's podman store\" in the operations guide (back up first, then " +
	"`podman system reset --force` as the jail user, then `lever up` without --fresh)"

func podmanStorageDamaged(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "storage corruption") ||
		strings.Contains(low, "may be corrupted") ||
		(strings.Contains(low, "readlink") && strings.Contains(low, "/overlay/l/")) ||
		strings.Contains(low, "layer not known")
}

// podmanErrTail returns the end of podman's output, sanitized and bounded,
// on one line. A line that names storage corruption is kept even when it
// falls before the tail, since it says what to do.
func podmanErrTail(out string) string {
	var lines []string
	for _, l := range strings.FieldsFunc(out, func(r rune) bool { return r == '\n' || r == '\r' }) {
		l = strings.TrimSpace(termsafe.Sanitize(l))
		if l == "" || isPodmanProgress(l) {
			continue
		}
		lines = append(lines, clipMiddle(l, podmanErrLineMax))
	}
	start := max(0, len(lines)-podmanErrTailLines)
	kept := lines[start:]
	var warn string
	for i := start - 1; i >= 0; i-- {
		if podmanStorageDamaged(lines[i]) {
			warn = lines[i]
			break
		}
	}
	omitted := start
	// The size counts the warning, the omission note (its longest form) and
	// every separator, so the result stays within podmanErrTailMax.
	size := func() int {
		n := len(warn) + 3 + len("(99999 earlier lines omitted) | ")
		for _, l := range kept {
			n += len(l) + 3
		}
		return n
	}
	for len(kept) > 1 && size() > podmanErrTailMax {
		kept = kept[1:]
		omitted++
	}
	parts := make([]string, 0, len(kept)+2)
	if warn != "" {
		parts = append(parts, warn)
		omitted--
	}
	if omitted > 0 {
		noun := "lines"
		if omitted == 1 {
			noun = "line"
		}
		parts = append(parts, fmt.Sprintf("(%d earlier %s omitted)", omitted, noun))
	}
	parts = append(parts, kept...)
	return strings.Join(parts, " | ")
}

func isPodmanProgress(l string) bool {
	for _, p := range podmanProgressPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// clipMiddle keeps more of the end of s than its front because an error
// line's cause is at its end.
func clipMiddle(s string, n int) string {
	const sep = " … "
	if len(s) <= n {
		return s
	}
	keep := n - len(sep)
	head, tail := keep/3, keep-keep/3
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	t := len(s) - tail
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	return s[:head] + sep + s[t:]
}

func podmanFailure(stderr, stdout string) string {
	out := stderr
	if strings.TrimSpace(stdout) != "" {
		out += "\n" + stdout
	}
	detail := podmanErrTail(out)
	if podmanStorageDamaged(out) {
		detail += " — " + StorageCorruptionHint
	}
	return detail
}
