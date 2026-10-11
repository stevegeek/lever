package jail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// loadFailureStderr has the shape of the field report (lever#173): podman
// load's copy progress first, podman's corruption warning, then the error,
// whose cause is at the very end of a long line.
func loadFailureStderr(blobs int) string {
	var b strings.Builder
	b.WriteString("Getting image source signatures\n")
	for i := range blobs {
		fmt.Fprintf(&b, "Copying blob sha256:%064d\r", i)
		fmt.Fprintf(&b, "Copying blob sha256:%064d done\n", i)
	}
	b.WriteString("Copying config sha256:abc done\n")
	b.WriteString("Writing manifest to image destination\n")
	b.WriteString("WARN[0042] Image abc exists in local storage, storage corruption might have occurred\n")
	b.WriteString("Error: payload does not match any of the supported image formats:\n")
	b.WriteString(" * oci: initializing source oci:/var/tmp/api_load: open /var/tmp/api_load/index.json: not a directory\n")
	b.WriteString(" * oci-archive: loading index: open /var/tmp/oci/index.json: no such file or directory\n")
	b.WriteString(" * docker-archive: writing blob: trying to reuse blob sha256:5f70bf18 at destination: reading layer \"0123\" for blob \"sha256:5f70bf18\": creating file-getter: readlink /home/u/.local/share/containers/storage/overlay/l/: no such file or directory\n")
	b.WriteString(" * dir: open /var/tmp/api_load/manifest.json: not a directory\n")
	return b.String()
}

func TestPodmanErrTailKeepsTheCauseNotTheProgress(t *testing.T) {
	got := podmanErrTail(loadFailureStderr(40))
	for _, want := range []string{
		"payload does not match any of the supported image formats",
		"readlink /home/u/.local/share/containers/storage/overlay/l/: no such file or directory",
		"storage corruption might have occurred",
		" * dir: open",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tail lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Copying blob", "Getting image source signatures", "Writing manifest", "\n", "\r"} {
		if strings.Contains(got, bad) {
			t.Errorf("tail keeps %q:\n%s", bad, got)
		}
	}
	if len(got) > podmanErrTailMax+200 {
		t.Errorf("tail is %d bytes, want about %d at most", len(got), podmanErrTailMax)
	}
}

func TestPodmanErrTailBoundsLinesAndBytes(t *testing.T) {
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "line %d %s\n", i, strings.Repeat("x", 2000))
	}
	got := podmanErrTail(b.String())
	if len(got) > podmanErrTailMax+64 {
		t.Fatalf("tail is %d bytes, cap %d", len(got), podmanErrTailMax)
	}
	if !strings.Contains(got, "line 499 ") {
		t.Errorf("tail lost the last line: %.200s", got)
	}
	if strings.Contains(got, "line 400 ") {
		t.Errorf("tail kept an early line")
	}
	if !strings.Contains(got, "earlier lines omitted)") {
		t.Errorf("tail does not count what it dropped: %.200s", got)
	}
	// Fewer lines than the limit, short: kept whole, no omission note.
	if got := podmanErrTail("Error: one\nError: two\n"); got != "Error: one | Error: two" {
		t.Errorf("short output = %q", got)
	}
	if got := podmanErrTail(""); got != "" {
		t.Errorf("empty output = %q", got)
	}
}

func TestPodmanErrTailStripsControlSequences(t *testing.T) {
	got := podmanErrTail("Error: \x1b[2Jbad\x1b]0;title\x07 thing\x00\n")
	if strings.ContainsAny(got, "\x1b\x07\x00") {
		t.Fatalf("control bytes survive: %q", got)
	}
	if !strings.Contains(got, "bad") || !strings.Contains(got, "thing") {
		t.Fatalf("text lost: %q", got)
	}
}

func TestClipMiddleKeepsHeadAndTail(t *testing.T) {
	s := "HEAD" + strings.Repeat("é", 1000) + "the cause at the end"
	got := clipMiddle(s, 120)
	if len(got) > 120 {
		t.Fatalf("len %d > 120", len(got))
	}
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "the cause at the end") {
		t.Fatalf("clip = %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("no cut marker: %q", got)
	}
	if got := clipMiddle("short", 120); got != "short" {
		t.Fatalf("short clipped: %q", got)
	}
}

func TestPodmanStorageDamaged(t *testing.T) {
	for in, want := range map[string]bool{
		"WARN storage corruption might have occurred":                               true,
		"creating file-getter: readlink /x/overlay/l/: no such file or directory":   true,
		"Image abc exists in local storage but may be corrupted (remove the image)": true,
		"Error: layer not known": true,
		"Error: short-name resolution enforced but cannot prompt without a TTY":         false,
		"Error: readlink /proc/self/exe: permission denied":                             false,
		"Error: payload does not match any of the supported image formats: * oci: nope": false,
	} {
		if got := podmanStorageDamaged(in); got != want {
			t.Errorf("podmanStorageDamaged(%q) = %v, want %v", in, got, want)
		}
	}
}

// stderrRunner fails podman load with a scripted stderr after draining stdin,
// the way a real load that reads the archive and then fails does.
type stderrRunner struct {
	proc.Runner
	stderr string
}

func (s stderrRunner) RunStdin(_ context.Context, in io.Reader, _ map[string]string, _ string, _ ...string) (proc.Result, error) {
	_, _ = io.Copy(io.Discard, in)
	return proc.Result{Code: 125, Stderr: s.stderr}, errors.New("exit status 125")
}

func TestLoadImageErrorCarriesPodmansLastLinesAndTheHint(t *testing.T) {
	err := loadImage(context.Background(), stderrRunner{stderr: loadFailureStderr(30)}, orbPrefix("m", "u"), "501", func(w io.Writer) error {
		_, err := io.WriteString(w, "tarball-bytes")
		return err
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"loadimage: podman load: exit status 125:",
		"overlay/l/: no such file or directory",
		"storage corruption might have occurred",
		"podman system reset --force",
		"lever doctor",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "Copying blob") {
		t.Errorf("error leads with the progress output:\n%s", msg)
	}
}

func TestLoadImageErrorWithoutDamageHasNoHint(t *testing.T) {
	err := loadImage(context.Background(), stderrRunner{stderr: "Getting image source signatures\nError: no space left on device\n"}, orbPrefix("m", "u"), "501", func(w io.Writer) error {
		_, err := io.WriteString(w, "x")
		return err
	})
	if err == nil || !strings.HasSuffix(err.Error(), "exit status 125: Error: no space left on device") {
		t.Fatalf("err = %v", err)
	}
}
