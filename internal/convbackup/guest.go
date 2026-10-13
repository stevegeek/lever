package convbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion/layout"
	"github.com/stevegeek/lever/internal/termsafe"
)

// Guest reaches the manager's home in the jail. The home is scion's, not
// lever's: <run user's home>/.scion/project-configs/<entry>/.scion/agents/
// <agent>/home (scion pkg/config/project_marker.go GetAgentHomePath), where
// <entry> is the one registration whose workspace_path is Workspace. The
// container sees it as /home/scion, so the agent can write anything inside
// it but nothing above it.
type Guest struct {
	R         proc.Runner // the jail transport, as the run user
	Workspace string      // the tree's mount point in the jail (Backend.MountDest)
	Agent     string      // the manager's scion slug (the app name)
}

// Exit codes of the guest scripts. Anything else is a failure.
const (
	exitNoHome      = 10 // no registration for the workspace, or no agent home
	exitNoProjects  = 11 // the home has no .claude/projects
	exitAmbiguous   = 12 // more than one registration claims the workspace
	exitNotPlainDir = 13 // .claude or .claude/projects is a link or not a directory
	exitTooLarge    = 14 // a file or the whole is past the limits
	exitNotSwapped  = 15 // a restore failed before the swap: projects is as it was
)

// maxCompressed bounds what a capture may print: the compressed archive. A
// command that prints more is killed (proc.WithOutputLimit).
const maxCompressed = 512 << 20

// captureTimeout bounds a capture; installTimeout a restore.
var (
	captureTimeout = 10 * time.Minute
	installTimeout = 10 * time.Minute
)

// homeScript resolves the agent home into $h and enters it, or exits with
// exitNoHome / exitAmbiguous. $1 is the workspace path and $2 the agent.
// Both are lever constants passed as arguments, never spliced into the
// script. The agent cannot reach anything here above its home, so the path
// up to it is not its to change.
const homeScript = `set -u
ws=$1; agent=$2
home=${HOME:-}
[ -n "$home" ] || { echo "HOME is not set in the jail" >&2; exit 2; }
entry=; n=0
for s in "$home"/` + layout.ProjectConfigsRel + `/*/` + layout.SettingsRel + `; do
  [ -f "$s" ] || continue
  wp=$(grep -E '^` + layout.WorkspacePathKey + `:' "$s" 2>/dev/null | head -1 | sed 's/^` + layout.WorkspacePathKey + `:[[:space:]]*//')
  if [ "$wp" = "$ws" ]; then entry=${s%/settings.yaml}; n=$((n+1)); fi
done
[ "$n" -le 1 ] || { echo "$n scion registrations claim $ws" >&2; exit 12; }
[ "$n" -eq 1 ] || exit 10
h=$entry/agents/$agent/home
[ -d "$h" ] && [ ! -L "$h" ] || exit 10
cd -P -- "$h" || exit 2
real=$(pwd -P)
`

// enterClaude enters the home's .claude, and refuses (exitNotPlainDir) when
// that is not a plain directory: cd -P resolves a link, and pwd -P then
// names the directory actually entered, whatever the agent swaps after.
// From here on every path is relative to that directory.
const enterClaude = `cd -P -- .claude 2>/dev/null && [ "$(pwd -P)" = "$real/.claude" ] || { echo ".claude in the manager's home is not a plain directory" >&2; exit 13; }
`

// captureScript prints a gzip tar of the home's .claude/projects. tar never
// follows a link inside it (the host drops links anyway), and the
// precheck refuses a file past the limits before tar spends minutes on it.
// tar's status 1 means a file changed while it was read (a manager still
// running): the archive is still whole, so it counts as success and is
// reported on stderr.
var captureScript = homeScript + `[ -e .claude ] || [ -L .claude ] || exit 11
` + enterClaude + `[ -e projects ] || [ -L projects ] || exit 11
[ -d projects ] && [ ! -L projects ] || { echo ".claude/projects in the manager's home is not a plain directory" >&2; exit 13; }
big=$(find projects -type f -size +` + fmt.Sprint(DefaultLimits.MaxFile/1024) + `k -print -quit 2>/dev/null)
[ -z "$big" ] || { echo "a file in .claude/projects is larger than the limit" >&2; exit 14; }
total=$(du -sk --apparent-size projects 2>/dev/null | cut -f1)
[ "${total:-0}" -le ` + fmt.Sprint(DefaultLimits.MaxTotal/1024) + ` ] || { echo ".claude/projects is larger than the limit" >&2; exit 14; }
tar -czf - projects
rc=$?
if [ "$rc" -eq 1 ]; then printf '\nLEVER-CHANGED\n' >&2; exit 0; fi
exit "$rc"
`

// installScript replaces the home's .claude/projects with the archive on
// stdin. It runs only while the manager is suspended (no process of the
// agent's runs), unpacks into a directory it has just created, and only then
// moves the current projects aside (renamed, never deleted: a rename moves a
// link, not what it points at) and the new one into place. A failed unpack
// leaves the current projects as they were. $3 is the stamp naming the
// directory moved aside.
var installScript = homeScript + `stamp=$3
if [ ! -e .claude ] && [ ! -L .claude ]; then mkdir -m 700 .claude || exit 15; fi
` + enterClaude + `new=projects.lever-restoring-$stamp
old=projects.lever-replaced-$stamp
mkdir -m 700 -- "$new" || exit 15
if ! ( cd -- "$new" && [ "$(pwd -P)" = "$real/.claude/$new" ] && tar -xzf - --no-same-owner --no-same-permissions ); then
  rm -rf -- "$new"
  echo "unpacking the conversation failed" >&2
  exit 15
fi
if [ -e projects ] || [ -L projects ]; then
  mv -T -- projects "$old" || { rm -rf -- "$new"; exit 15; }
  echo "PARKED $old"
fi
if ! mv -T -- "$new" projects; then
  if [ -e "$old" ] || [ -L "$old" ]; then mv -T -- "$old" projects || exit 2; fi
  rm -rf -- "$new"
  exit 15
fi
`

// transcriptsScript prints what the home's .claude/projects holds, as one
// marker line: LEVER-NONE when there is no .claude, no projects, or a
// projects (a plain directory) holding no regular file with any content and
// nothing but directories and regular files; LEVER-SOME when it holds a
// regular file with content; LEVER-OTHER when it holds a link or a special
// file, or when .claude holds another entry whose name starts with
// "projects" (a restore's parked transcripts, say). It reads names and sizes
// only, follows no link (find does not, and enterClaude refuses a linked
// .claude), and fails on any find error (an unreadable directory may hide a
// transcript).
var transcriptsScript = homeScript + `[ -e .claude ] || [ -L .claude ] || { echo LEVER-NONE; exit 0; }
` + enterClaude + `for e in projects?*; do
  [ -e "$e" ] || [ -L "$e" ] || continue
  echo LEVER-OTHER; exit 0
done
[ -e projects ] || [ -L projects ] || { echo LEVER-NONE; exit 0; }
[ -d projects ] && [ ! -L projects ] || { echo ".claude/projects in the manager's home is not a plain directory" >&2; exit 13; }
other=$(find projects ! -type d ! -type f -print -quit) || exit 2
[ -z "$other" ] || { echo LEVER-OTHER; exit 0; }
some=$(find projects -type f -size +0c -print -quit) || exit 2
[ -z "$some" ] || { echo LEVER-SOME; exit 0; }
echo LEVER-NONE
`

// Transcripts is what Guest.Transcripts found in the manager's home.
type Transcripts int

const (
	// TranscriptsNone: no .claude, no .claude/projects, or one that holds
	// only directories and empty regular files, and no other "projects*"
	// entry in .claude. The home holds no conversation.
	TranscriptsNone Transcripts = iota + 1
	// TranscriptsSome: .claude/projects holds a regular file with content.
	TranscriptsSome
	// TranscriptsOther: a link or a special file in .claude/projects, or
	// another "projects*" entry in .claude. Not proof either way.
	TranscriptsOther
)

// transcriptsTimeout bounds Guest.Transcripts.
var transcriptsTimeout = time.Minute

// Transcripts looks, from the guest, at what the manager's home holds in
// .claude/projects (see TranscriptsNone and the others), reading names and
// sizes only. A missing home is ErrNoHome, a linked .claude or projects
// ErrNotPlainDir; any other failure is an error. Only TranscriptsNone with a
// nil error proves the home holds no conversation.
func (g Guest) Transcripts(ctx context.Context) (Transcripts, error) {
	ctx, cancel := context.WithTimeout(ctx, transcriptsTimeout)
	defer cancel()
	ctx = proc.WithOutputLimit(ctx, 4<<10)
	res, err := g.R.Run(ctx, nil, "bash", "--noprofile", "--norc", "-c", transcriptsScript, "lever-conversation", g.Workspace, g.Agent)
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("exit status %d", res.Code)
	}
	if err != nil {
		switch res.Code {
		case exitNoHome:
			return 0, ErrNoHome
		case exitNotPlainDir:
			return 0, ErrNotPlainDir
		}
		return 0, fmt.Errorf("reading the manager's transcripts in the jail: %v%s", err, stderrTail(res.Stderr))
	}
	switch strings.TrimSpace(res.Stdout) {
	case "LEVER-NONE":
		return TranscriptsNone, nil
	case "LEVER-SOME":
		return TranscriptsSome, nil
	case "LEVER-OTHER":
		return TranscriptsOther, nil
	}
	return 0, errors.New("reading the manager's transcripts in the jail: unexpected output")
}

// ErrNoHome means lever found no home for the manager in the jail: no
// registration claims the workspace, or the record has no home directory.
var ErrNoHome = errors.New("lever found no home for the manager in the jail")

// ErrNotSwapped wraps an Install error that is known to have left the
// manager's .claude/projects as it was. Any other Install error may have
// come after the swap.
var ErrNotSwapped = errors.New("the conversation in the manager's home was not changed")

// stampRE is the shape Install accepts for its stamp.
var stampRE = regexp.MustCompile(`^[0-9A-Za-z-]{1,40}$`)

// ErrNotPlainDir means the agent replaced .claude or .claude/projects in its
// home with a link (or something that is not a directory). lever does not
// follow it.
var ErrNotPlainDir = errors.New("the manager's .claude/projects is not a plain directory in its home; lever does not follow links the agent made")

// Capture reads the manager's .claude/projects from the guest as a gzip tar
// archive (names beginning "projects"), returned as a string to spare a
// copy of what can be hundreds of megabytes. found is false when the home
// has no projects directory: nothing to back up. A missing home is
// ErrNoHome. changed reports that a file changed while it was read (the
// manager was running).
func (g Guest) Capture(ctx context.Context) (archive string, found, changed bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, captureTimeout)
	defer cancel()
	ctx = proc.WithOutputLimit(ctx, maxCompressed)
	res, err := g.R.Run(ctx, nil, "bash", "--noprofile", "--norc", "-c", captureScript, "lever-conversation", g.Workspace, g.Agent)
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("exit status %d", res.Code)
	}
	switch {
	case err == nil:
		return res.Stdout, true, hasLine(res.Stderr, "LEVER-CHANGED"), nil
	case errors.Is(err, proc.ErrOutputLimit):
		return "", false, false, fmt.Errorf("reading the manager's conversation printed more than %d MiB (the compressed archive, or warnings); nothing was saved", maxCompressed>>20)
	}
	switch res.Code {
	case exitNoHome:
		return "", false, false, ErrNoHome
	case exitNoProjects:
		return "", false, false, nil
	case exitNotPlainDir:
		return "", false, false, ErrNotPlainDir
	case exitTooLarge:
		return "", false, false, fmt.Errorf("the manager's conversation is past lever's limits (%d MiB a file, %d MiB in all)", DefaultLimits.MaxFile>>20, DefaultLimits.MaxTotal>>20)
	}
	return "", false, false, fmt.Errorf("reading the manager's conversation in the jail: %v%s", err, stderrTail(res.Stderr))
}

// hasLine reports whether s holds line as a whole line.
func hasLine(s, line string) bool {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// Install puts archive (a stored backup: names relative to projects) into
// the manager's home as .claude/projects, moving the current one aside as
// .claude/projects.lever-replaced-<stamp>. The caller suspends the manager
// first. parked names what was moved aside ("" when there was nothing).
func (g Guest) Install(ctx context.Context, archive io.Reader, stamp string) (parked string, err error) {
	if !stampRE.MatchString(stamp) {
		return "", fmt.Errorf("conversation restore: bad stamp %q", stamp)
	}
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	ctx = proc.WithOutputLimit(ctx, 64<<10)
	res, err := g.R.RunStdin(ctx, archive, nil, "bash", "--noprofile", "--norc", "-c", installScript, "lever-conversation", g.Workspace, g.Agent, stamp)
	if err == nil && res.Code != 0 {
		err = fmt.Errorf("exit status %d", res.Code)
	}
	if err != nil {
		switch res.Code {
		case exitNoHome:
			return "", fmt.Errorf("%w: %w (is its record there? `lever up` first)", ErrNotSwapped, ErrNoHome)
		case exitNotPlainDir:
			return "", fmt.Errorf("%w: %w", ErrNotSwapped, ErrNotPlainDir)
		case exitNotSwapped:
			return "", fmt.Errorf("%w: writing the conversation into the jail failed%s", ErrNotSwapped, stderrTail(res.Stderr))
		}
		// A timeout, a lost transport or anything else: the guest may have
		// swapped the files anyway.
		return "", fmt.Errorf("writing the conversation into the jail: %v%s", err, stderrTail(res.Stderr))
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "PARKED "); ok {
			parked = ".claude/" + p
		}
	}
	return parked, nil
}

// stderrTail is the last line of a guest script's stderr, made safe to
// print, as ": <line>" ("" when empty).
func stderrTail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return ": " + termsafe.Sanitize(s)
}
