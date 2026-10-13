package apply

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

// staleBackgroundPhrases are what Claude Code (2.1.296) prints when claude
// --continue refuses because the last conversation moved into a background
// session. The first is printed after a container restart: the session was
// registered in another container.
var staleBackgroundPhrases = []string{
	"moved to a background session",
	"most recent conversation is running in the background",
}

// StaleBackgroundHint is the cause and the recovery lever prints when a
// manager's harness died because of a stale background session.
const StaleBackgroundHint = "cause: Claude refused to continue, because the last conversation was moved into a " +
	"background session (/background, /bg or the left-arrow key) and its session records in the agent home " +
	"name another container. lever turns background sessions off at every start, but records left from " +
	"before still block claude --continue. Recovery: in the agent home (/home/scion in the manager's " +
	"container), move aside ~/.claude/sessions/*.json records that hold \"kind\":\"bg\" or a \"parkedJobId\", " +
	"and ~/.claude/jobs/ and ~/.claude/daemon*; the conversation transcript is kept. Then lever stop && " +
	"lever up. `lever doctor` lists such records while the container runs; see the operations guide, " +
	"\"Background sessions and /login\""

// noConversationPhrases are what Claude Code prints when claude --continue
// finds no transcript in the agent home: scion resumes with --continue, so a
// manager that was never used exits 1.
var noConversationPhrases = []string{
	"No conversation found to continue",
}

// NoConversationHint is the cause and the recovery lever prints when a
// manager's harness died because claude --continue found no conversation.
// The log cannot prove the home holds no transcript, hence "most likely";
// `lever up` proves it from the guest before starting fresh on its own
// (Deps.ManagerConversationEmpty).
const NoConversationHint = "cause: no conversation to continue: Claude found none (scion resumes the manager with " +
	"claude --continue), so the manager was most likely never used. `lever up --fresh` starts it fresh and loses " +
	"nothing: it backs up any transcript in the agent home first (`lever conversation list`)"

// ErrNoConversation marks a manager liveness failure whose cause is
// NoConversationHint; its text is the hint.
var ErrNoConversation = errors.New(NoConversationHint)

type harnessCause int

const (
	causeNone harnessCause = iota
	causeStaleBackground
	causeNoConversation
)

// harnessCauseOf classifies the log tail of an agent container whose harness
// died. The tail is agent-controlled: only fixed phrases are matched and
// nothing of it is returned. When several causes match (a container restarted
// in place keeps its earlier output), the one printed last wins. The tail is
// a tmux terminal stream, so whitespace and control sequences are squashed
// out of both sides: a phrase split by pane wrapping still matches.
func harnessCauseOf(logTail string) harnessCause {
	norm := squashForMatch(logTail)
	best, at := causeNone, -1
	for _, c := range []struct {
		cause   harnessCause
		phrases []string
	}{
		{causeStaleBackground, staleBackgroundPhrases},
		{causeNoConversation, noConversationPhrases},
	} {
		for _, p := range c.phrases {
			if i := strings.LastIndex(norm, squashForMatch(p)); i > at {
				best, at = c.cause, i
			}
		}
	}
	return best
}

// HarnessDeathHint returns the hint to print for the log tail of an agent
// container whose harness died, or "" when it knows no cause. A tmux server
// crash is named only when no other cause is. Nothing of the tail is
// returned.
func HarnessDeathHint(logTail string) string {
	switch harnessCauseOf(logTail) {
	case causeStaleBackground:
		return StaleBackgroundHint
	case causeNoConversation:
		return NoConversationHint
	}
	return tmuxServerDeathHint(squashForMatch(logTail))
}

// NoConversationInLog reports whether a missing conversation is the latest
// known cause of a harness death in the log tail.
func NoConversationInLog(logTail string) bool {
	return harnessCauseOf(logTail) == causeNoConversation
}

func squashForMatch(s string) string {
	s = stripTerminalControls(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// stripTerminalControls removes ECMA-48 escape sequences, including control
// strings ended by BEL or ST. An unterminated sequence at the end is dropped.
// Other control characters are left for the caller.
func stripTerminalControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b && i+1 < len(s) && s[i+1] == '[':
			i = skipCSI(s, i+2)
		case c == 0x1b && i+1 < len(s) && strings.IndexByte("]PX^_", s[i+1]) >= 0:
			i = skipString(s, i+2)
		case c == 0x1b && i+1 < len(s):
			// ESC, intermediate bytes, one final byte (ESC 7, ESC ( B).
			i++
			for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
				i++
			}
			if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
				i++
			}
		case c == 0x1b:
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func skipCSI(s string, i int) int {
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
		i++
	}
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		i++
	}
	return i
}

func skipString(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
			return i + 2
		}
		i++
	}
	return i
}

// withHarnessDeathHint adds a known cause to a manager liveness failure. The
// container's session records are read first, since the container often
// outlives the harness (tmux's shell window keeps it running); the log tail
// is the fallback. A missing conversation wraps ErrNoConversation so
// start-manager can tell it apart. err stays wrapped, so errors.Is still finds
// the gate's sentinel.
func withHarnessDeathHint(ctx context.Context, d Deps, err error, project, name string) error {
	if err == nil {
		return err
	}
	if !errors.Is(err, scion.ErrAgentDied) && !errors.Is(err, scion.ErrAgentNotLive) {
		return err
	}
	ref := jail.ContainerName(path.Base(project), name)
	if d.BackgroundSessions != nil {
		if recs, berr := d.BackgroundSessions(ctx, ref); berr == nil && recs.Stale > 0 {
			return fmt.Errorf("%w — %s", err, StaleBackgroundHint)
		}
	}
	if d.HarnessLogTail == nil {
		return err
	}
	tail, rerr := d.HarnessLogTail(ctx, ref)
	if rerr != nil && tail == "" {
		return err
	}
	switch harnessCauseOf(tail) {
	case causeStaleBackground:
		return fmt.Errorf("%w — %s", err, StaleBackgroundHint)
	case causeNoConversation:
		return fmt.Errorf("%w — %w", err, ErrNoConversation)
	}
	if h := tmuxServerDeathHint(squashForMatch(tail)); h != "" {
		return fmt.Errorf("%w — %s", err, h)
	}
	return err
}
