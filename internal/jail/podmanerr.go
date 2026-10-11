package jail

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stevegeek/lever/internal/termsafe"
)

// The bounds on the podman output an error carries (lever#173). podman load
// writes its progress ("Copying blob …") to stderr before it fails, and the
// cause comes last, so an error that relays the whole stream leads with the
// progress and buries the cause. podmanErrTail keeps the last lines instead.
const (
	podmanErrTailLines = 12   // lines kept from the end
	podmanErrLineMax   = 600  // bytes kept of one line: its head and its tail
	podmanErrTailMax   = 4096 // bytes kept in all; earlier lines go first
)

// podmanProgressPrefixes are the progress lines podman (containers/image)
// writes while it copies an image. They say nothing about a failure and are
// dropped before the tail is taken.
var podmanProgressPrefixes = []string{
	"Getting image source signatures",
	"Copying blob ",
	"Copying config ",
	"Writing manifest to image destination",
	"Storing signatures",
}

// StorageCorruptionHint is what lever adds to a podman error that names
// damage in the jail's container store.
const StorageCorruptionHint = "the jail's podman store looks damaged: run `lever doctor` (podman storage row) " +
	"and follow \"Recovering the jail's podman store\" in the operations guide (back up first, then " +
	"`podman system reset --force` as the jail user, then `lever up` without --fresh)"

// podmanStorageDamaged reports whether podman output names damage in the
// container store: podman's own corruption warning, a layer whose overlay
// link is gone (the readlink of .../overlay/l/…), or a layer the store no
// longer knows.
func podmanStorageDamaged(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "storage corruption") ||
		strings.Contains(low, "may be corrupted") ||
		(strings.Contains(low, "readlink") && strings.Contains(low, "/overlay/l/")) ||
		strings.Contains(low, "layer not known")
}

// podmanErrTail returns the end of podman's output, fit for an error: split
// into lines (on \n and on the \r a progress bar redraws with), progress lines
// dropped, each line sanitized for the terminal (termsafe.Sanitize) and
// trimmed, the last podmanErrTailLines kept, each clipped to podmanErrLineMax
// bytes and the whole to podmanErrTailMax. The lines are joined with " | "
// because a returned error is printed on one line. A line that names storage
// corruption is kept even when it falls before the tail, since it is the line
// that says what to do. Omitted lines are counted at the front.
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
	// The corruption warning podman prints ahead of the error.
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

// clipMiddle shortens s to at most n bytes by cutting its middle, keeping a
// third at the front and the rest at the end: an error line's cause (the
// readlink path, "no such file or directory") is at its end. Cuts fall on
// rune boundaries.
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

// podmanFailure composes the detail of a failed podman command for an error:
// the tail of stderr then stdout, and the corruption hint when the output
// names store damage.
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
