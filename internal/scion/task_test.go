package scion

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// shellQuote replicates scion's pkg/runtime/common.go shellQuote — the ONE
// quoting pass that reaches tmux (scion's second pass is consumed by the
// container's own `sh -c` before tmux parses the command). TaskArgvBytes must
// equal that quoted word minus its two wrapping quotes, which the fixed part
// of TaskArgvBudget already accounts for.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func TestTaskArgvBytesMatchesScionsQuotingPass(t *testing.T) {
	for _, task := range []string{"", "abc", "it's", "a'b'c", strings.Repeat("'", 10) + "x"} {
		want := len(shellQuote(task)) - 2
		if got := TaskArgvBytes(task); got != want {
			t.Fatalf("TaskArgvBytes(%q) = %d, want %d (scion's quoted word minus its two quotes)", task, got, want)
		}
	}
	// Golden from a measured run of scion's construction through a real shell
	// (review of lever#30): ten apostrophes add 40 bytes to the argv sum tmux
	// receives, ten plain characters add 10.
	if d := TaskArgvBytes(strings.Repeat("'", 10)) - TaskArgvBytes(strings.Repeat("a", 10)); d != 30 {
		t.Fatalf("ten apostrophes must cost 30 bytes more than ten plain chars, got %d", d)
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

// Apostrophes count against the budget at their quoted size: a task under the
// budget in bytes can still be over it once scion quotes it.
func TestCheckTaskChargesApostrophes(t *testing.T) {
	n := TaskArgvBudget/4 + 1 // 4 bytes each once quoted
	if err := CheckTask(strings.Repeat("'", n)); err == nil {
		t.Fatalf("%d apostrophes (%d argv bytes) must exceed the %d budget", n, TaskArgvBytes(strings.Repeat("'", n)), TaskArgvBudget)
	}
}

func TestCheckInstructions(t *testing.T) {
	if err := CheckInstructions(""); err != nil {
		t.Fatalf("empty means no config and must pass: %v", err)
	}
	if err := CheckInstructions(strings.Repeat("m", MaxInstructionsBytes)); err != nil {
		t.Fatalf("exactly at the cap must pass: %v", err)
	}
	if err := CheckInstructions(strings.Repeat("m", MaxInstructionsBytes+1)); err == nil || !strings.Contains(err.Error(), strconv.Itoa(MaxInstructionsBytes)) {
		t.Fatalf("one byte over the cap must fail naming the cap, got %v", err)
	}
	// scion resolves a leading file:// as a file reference before provisioning
	// (pkg/api/content.go ResolveContent): refuse rather than stage some other
	// file — or nothing — as the agent's standing instructions.
	for _, bad := range []string{"file:///etc/passwd", "file://relative.md"} {
		if err := CheckInstructions(bad); err == nil || !strings.Contains(err.Error(), "file://") {
			t.Fatalf("%q must be refused by name, got %v", bad, err)
		}
	}
	if err := CheckInstructions("# Manual\nsee file://x later\n"); err != nil {
		t.Fatalf("file:// anywhere but the start is plain text: %v", err)
	}
}

// A task is never a flag. scion's start parses flags interspersed with
// positionals, so a task beginning with "-" is refused by name (lever's argv
// puts it behind `--` anyway — this is the second layer). Leading whitespace
// or a dash later in the text is not a flag to cobra and passes.
func TestCheckTaskRefusesLeadingDash(t *testing.T) {
	for _, task := range []string{"-", "--", "-b", "--config=/lever/x.json", "--no-auth"} {
		err := CheckTask(task)
		var fe *TaskFlagError
		if !errors.As(err, &fe) {
			t.Fatalf("CheckTask(%q) = %v, want *TaskFlagError", task, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "flag") || !strings.Contains(msg, "-") {
			t.Fatalf("CheckTask(%q) error must name the reason, got %q", task, msg)
		}
	}
	for _, task := range []string{"", "do x", " -not a flag", "fix --config handling", "a\n- list"} {
		if err := CheckTask(task); err != nil {
			t.Fatalf("CheckTask(%q) = %v, want nil", task, err)
		}
	}
}

// The size rule still holds for a task that also begins with "-": whichever
// error comes first, an over-budget task never passes.
func TestCheckTaskSizeRuleSurvivesFlagRule(t *testing.T) {
	if err := CheckTask(strings.Repeat("x", TaskArgvBudget+1)); err == nil {
		t.Fatal("over-budget task must still be refused")
	}
	if err := CheckTask(strings.Repeat("-", TaskArgvBudget+1)); err == nil {
		t.Fatal("over-budget flag-shaped task must be refused")
	}
}

// TestCheckTaskFlagRuleIsShapeNotPrefix: only a flag SHAPE is refused. A
// markdown list item or a negative number starts with "-" and is a task.
func TestCheckTaskFlagRuleIsShapeNotPrefix(t *testing.T) {
	for _, task := range []string{"- do the thing\n- then this", "-1 is the answer", "-. odd but not a flag"} {
		if err := CheckTask(task); err != nil {
			t.Errorf("CheckTask(%q) = %v, want nil (not flag-shaped)", task, err)
		}
	}
	for _, task := range []string{"-b", "--config=/lever/x.json", "--", "-", "---", "--no-auth rest", "-a rest"} {
		var fe *TaskFlagError
		if err := CheckTask(task); !errors.As(err, &fe) {
			t.Errorf("CheckTask(%q) = %v, want *TaskFlagError", task, err)
		}
	}
}
