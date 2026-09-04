package scion

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// scion single-quotes the task twice on its way into the tmux command line
// (once as a harness argv word, once more inside the `sh -c` wrapper), so each
// apostrophe expands to 13 bytes there — 12 more than the byte itself.
func TestTaskArgvBytesCountsDoubleQuotedApostrophes(t *testing.T) {
	if got := TaskArgvBytes("abc"); got != 3 {
		t.Fatalf("TaskArgvBytes(abc) = %d, want 3", got)
	}
	if got := TaskArgvBytes("it's"); got != 4+12 {
		t.Fatalf("TaskArgvBytes(it's) = %d, want %d", got, 4+12)
	}
}

func TestCheckTaskBoundary(t *testing.T) {
	atBudget := strings.Repeat("a", TaskArgvBudget)
	if err := CheckTask(atBudget); err != nil {
		t.Fatalf("a task exactly at the budget must pass: %v", err)
	}
	err := CheckTask(atBudget + "a")
	var tl *TaskTooLongError
	if !errors.As(err, &tl) {
		t.Fatalf("one byte over the budget must fail with *TaskTooLongError, got %v", err)
	}
	if tl.Bytes != TaskArgvBudget+1 || tl.Budget != TaskArgvBudget {
		t.Fatalf("error carries Bytes=%d Budget=%d, want %d/%d", tl.Bytes, tl.Budget, TaskArgvBudget+1, TaskArgvBudget)
	}
	// The message is the whole diagnosis: the numbers, the cause, and the fix.
	for _, want := range []string{strconv.Itoa(TaskArgvBudget + 1), strconv.Itoa(TaskArgvBudget), "tmux", "instructions_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must mention %q", err, want)
		}
	}
}

// Apostrophes count against the budget at their expanded size: a task well
// under the budget in bytes can still be over it once scion quotes it.
func TestCheckTaskChargesApostrophes(t *testing.T) {
	// 13 bytes per apostrophe after quoting; enough of them push a short task over.
	n := TaskArgvBudget/13 + 1
	if err := CheckTask(strings.Repeat("'", n)); err == nil {
		t.Fatalf("%d apostrophes (%d argv bytes) must exceed the %d budget", n, TaskArgvBytes(strings.Repeat("'", n)), TaskArgvBudget)
	}
}
