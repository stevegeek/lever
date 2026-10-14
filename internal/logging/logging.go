// Package logging is the log line shape of lever's long-running processes:
// slog text lines with the time in UTC to the millisecond, a component
// field, and credentials masked (Redact) in the message and every string
// field. Install also routes the standard log package, and so net/http's
// error lines, through the same handler.
//
// Messages for a person at a terminal are not log lines and keep their
// prose form: "<component>: <message>", "<component>: warning: <message>".
package logging

import (
	"bytes"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// TimeFormat is the time field's layout.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// New returns a logger in lever's daemon shape writing to w.
func New(w io.Writer, component string) *slog.Logger {
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: replace})
	return slog.New(h).With("component", component)
}

var installed atomic.Bool

// Install makes New(w, component) the default logger of slog and of the
// standard log package. Call it once, first thing, in a daemon's main.
func Install(w io.Writer, component string) *slog.Logger {
	l := New(w, component)
	slog.SetDefault(l)
	installed.Store(true)
	return l
}

// Installed reports whether Install ran, that is whether a warning belongs
// in a daemon log rather than on a terminal.
func Installed() bool { return installed.Load() }

func replace(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		return slog.String(slog.TimeKey, a.Value.Time().UTC().Format(TimeFormat))
	}
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); s != "" {
			if r := Redact(s); r != s {
				return slog.String(a.Key, r)
			}
		}
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok && err != nil {
			return slog.String(a.Key, Redact(err.Error()))
		}
	}
	return a
}

// The credential shapes Redact masks. Each keeps a recognisable prefix so
// a reader knows what was there. The rules match a credential's shape, not
// a word like "token" in prose: "reading token /path failed" stays as it is.
var (
	// envValueRE keeps the key of a container-runtime -e/--env argument and
	// masks the value.
	envValueRE = regexp.MustCompile(`(^|\s)(-e|--env)([ =])([A-Za-z_][A-Za-z0-9_]*=)\S*`)
	// anthropicRE: Claude Code OAuth tokens and API keys.
	anthropicRE = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`)
	githubRE    = regexp.MustCompile(`\b(gh[pousr]_|github_pat_)[A-Za-z0-9_]{16,}`)
	// scionPATRE: the scion hub's personal access tokens.
	scionPATRE   = regexp.MustCompile(`\b(scion_pat_)[A-Za-z0-9_-]{8,}`)
	jwtRE        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\.?[A-Za-z0-9_-]*`)
	authHeaderRE = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-[a-z0-9-]*token)(\s*[:=]\s*)(bearer\s+|basic\s+)?\S+`)
	bearerRE     = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]{16,}`)
	secretFlagRE = regexp.MustCompile(`(?i)(--?[a-z0-9-]*(secret|token|password)[a-z0-9-]*)(=|\s+)([^\s-]\S*)`)
)

// Redact masks the credentials lever handles wherever they appear in s.
// It is a backstop: code should not log a credential in the first place.
func Redact(s string) string {
	s = envValueRE.ReplaceAllString(s, "${1}${2}${3}${4}***")
	s = anthropicRE.ReplaceAllString(s, "sk-ant-***")
	s = githubRE.ReplaceAllString(s, "${1}***")
	s = scionPATRE.ReplaceAllString(s, "${1}***")
	s = jwtRE.ReplaceAllString(s, "eyJ***")
	s = authHeaderRE.ReplaceAllString(s, "${1}${2}${3}***")
	s = bearerRE.ReplaceAllString(s, "${1} ***")
	return secretFlagRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretFlagRE.FindStringSubmatch(m)
		// A flag naming where the secret lives (--token-file, --secret-dir)
		// carries a path, not the secret.
		for _, suffix := range []string{"-file", "-path", "-dir", "-env", "-name"} {
			if strings.HasSuffix(strings.ToLower(sub[1]), suffix) {
				return m
			}
		}
		return sub[1] + sub[3] + "***"
	})
}

// Writer returns an io.Writer that logs each line through l, so a daemon's
// operator prose takes the log shape too. A "<component>: warning: <text>"
// line is logged at WARN, "<component>: error: <text>" at ERROR, anything
// else at INFO.
func Writer(l *slog.Logger) io.Writer { return &lineWriter{l: l} }

type lineWriter struct {
	mu  sync.Mutex
	l   *slog.Logger
	buf []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:i]), "\r")
		w.buf = w.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			logProse(w.l, line)
		}
	}
	// A line longer than this without a newline is logged as it stands.
	if len(w.buf) > 64<<10 {
		logProse(w.l, string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}

var proseLevelRE = regexp.MustCompile(`^([a-z][a-z0-9 -]{0,30}): (?i:(warning|error)): `)

func logProse(l *slog.Logger, line string) {
	if m := proseLevelRE.FindStringSubmatch(line); m != nil {
		text := line[len(m[0]):]
		if strings.EqualFold(m[2], "error") {
			l.Error(text, "from", m[1])
		} else {
			l.Warn(text, "from", m[1])
		}
		return
	}
	l.Info(line)
}
