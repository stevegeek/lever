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

// staleBackgroundPhrases are what Claude Code prints when claude --continue
// refuses because the last conversation moved into a background session
// (Claude Code 2.1.281). The first is the case after a container restart:
// the session was registered in another container, so this one cannot tell
// whether it still runs.
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
// finds no conversation to continue in the agent home: a manager that was
// started and never sent anything has no transcript, so scion's resume of it
// (which passes --continue) ends there and the harness exits 1 (lever#202).
var noConversationPhrases = []string{
	"No conversation found to continue",
}

// NoConversationHint is the cause and the recovery lever prints when a
// manager's harness died because claude --continue found no conversation.
// The log says nothing about whether the home holds a transcript elsewhere,
// so this says "most likely"; `lever up` proves it from the guest before it
// starts the manager fresh on its own (Deps.ManagerConversationEmpty).
const NoConversationHint = "cause: no conversation to continue: Claude found none (scion resumes the manager with " +
	"claude --continue), so the manager was most likely never used. `lever up --fresh` starts it fresh and loses " +
	"nothing: it backs up any transcript in the agent home first (`lever conversation list`)"

// ErrNoConversation marks a manager liveness failure whose cause is
// NoConversationHint; its text is the hint.
var ErrNoConversation = errors.New(NoConversationHint)

// harnessCause is a known cause of a harness death, read from its log.
type harnessCause int

const (
	causeNone harnessCause = iota
	causeStaleBackground
	causeNoConversation
)

// harnessCauseOf classifies the log tail of an agent container whose harness
// died. The tail is the agent's word: only fixed phrases are matched, and
// nothing of it is returned. When it holds the phrases of more than one
// cause (a container restarted in place keeps its earlier output), the one
// printed last wins.
//
// The tail is a terminal stream (the harness runs in tmux), so it is
// normalised before the match: terminal control sequences are removed, case
// is folded, and all whitespace is dropped from both the tail and the
// phrases. A phrase split by pane wrapping (a newline, or cursor movement
// in place of spaces) still matches.
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

// HarnessDeathHint classifies the log tail of an agent container whose
// harness died (harnessCauseOf) and returns the hint to print, or "" when it
// knows no cause. A tmux server crash that sciontool logged (lever#201) is
// named when no other cause is. Nothing of the tail is returned.
func HarnessDeathHint(logTail string) string {
	switch harnessCauseOf(logTail) {
	case causeStaleBackground:
		return StaleBackgroundHint
	case causeNoConversation:
		return NoConversationHint
	}
	return tmuxServerDeathHint(squashForMatch(logTail))
}

// NoConversationInLog reports whether the log tail of an agent container
// names a missing conversation as the latest known cause of its harness's
// death (harnessCauseOf). Doctor's manager row uses it.
func NoConversationInLog(logTail string) bool {
	return harnessCauseOf(logTail) == causeNoConversation
}

// squashForMatch removes terminal control sequences and every whitespace
// and control character from s, and folds it to lower case.
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

// stripTerminalControls removes ANSI/ECMA-48 escape sequences from s: CSI
// (ESC [ ... final byte), OSC, DCS, SOS, PM and APC strings (ended by BEL or
// ESC \), and the other ESC sequences (ESC 7, ESC ( B). An unterminated
// sequence at the end is dropped. Other control characters are left for the
// caller.
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

// skipCSI returns the index after the CSI sequence whose parameters start
// at i: parameter and intermediate bytes, then one final byte (0x40-0x7e).
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

// skipString returns the index after the control string whose body starts
// at i, ended by BEL or ST (ESC \); the end of s when it is unterminated.
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

// withHarnessDeathHint adds a known cause to a manager liveness failure, when
// the gate saw the harness die (or never come up). The primary signal is the
// container's own session records: the container often outlives the harness
// (tmux's shell window keeps it running), and d.BackgroundSessions counts
// stale background records in it (jail.BackgroundRecords.Stale). When that
// finds none, cannot run (no container, no reader) or fails, the fallback
// matches fixed phrases in the container log tail (d.HarnessLogTail,
// HarnessDeathHint). A missing conversation also wraps ErrNoConversation, so
// start-manager can tell it apart (startFreshWithoutConversation). Any other
// error, or no known cause, returns err as it is. err stays wrapped, so
// errors.Is still finds the gate's sentinel.
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
