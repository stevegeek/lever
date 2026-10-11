package scion

import (
	"regexp"
	"strings"
	"sync"
)

// settingsKeysWarnRE matches, as a whole line, the warning scion's settings
// loader prints on every CLI call against a v1 (schema_version "1") settings
// file (lever#169):
//
//	2026/09/22 21:39:27 WARN settings file contains unrecognized keys (these will be ignored) keys="[hub.project_id runtimes[*].type]" path=...
//
// It is harmless. scion's legacy-format probe decodes the file into the
// legacy Settings struct before the versioned loader runs; the versioned
// loader reads every key the probe lists, and hub.project_id is written by
// scion itself. The line is scion's default slog logger: an optional
// log.LstdFlags timestamp, the level, the message, then key=value attributes.
//
// Only this one message is matched — the "settings" label, the exact text and
// nothing but attributes after it — so any other scion warning, including the
// same message for a different file (the hub or server config), still
// reaches the operator.
var settingsKeysWarnRE = regexp.MustCompile(
	`^(?:\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} )?WARN settings file contains unrecognized keys \(these will be ignored\)(?: [a-z_]+=.*)?$`)

// IsSettingsKeysWarning reports whether line is scion's benign
// unrecognized-settings-keys warning (see settingsKeysWarnRE). Surrounding
// whitespace and terminal colour codes are ignored.
func IsSettingsKeysWarning(line string) bool {
	return settingsKeysWarnRE.MatchString(strings.TrimSpace(ansiRE.ReplaceAllString(line, "")))
}

// splitSettingsWarnings removes every settings-keys warning line from scion
// output and returns the rest, with the removed lines in order. Text without
// a match comes back unchanged.
func splitSettingsWarnings(output string) (kept string, dropped []string) {
	if !strings.Contains(output, "settings file contains unrecognized keys") {
		return output, nil
	}
	lines := strings.Split(output, "\n")
	keep := make([]string, 0, len(lines))
	for _, l := range lines {
		if IsSettingsKeysWarning(l) {
			dropped = append(dropped, strings.TrimSpace(l))
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n"), dropped
}

// warnLog records the settings warnings the client filtered out of what it
// shows the operator, so they stay in the log file. scion prints the same
// warning on every call, so a line already recorded by this client (compared
// without its timestamp) is not recorded again.
type warnLog struct {
	sink func(line string)
	mu   sync.Mutex
	seen map[string]bool
}

var lstdTimestampRE = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

func (w *warnLog) record(lines []string) {
	if w == nil || w.sink == nil || len(lines) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range lines {
		key := lstdTimestampRE.ReplaceAllString(l, "")
		if w.seen[key] {
			continue
		}
		if w.seen == nil {
			w.seen = map[string]bool{}
		}
		w.seen[key] = true
		w.sink(l)
	}
}

// filterOutput drops the settings warnings from raw scion output and hands
// them to the log sink. Every path that turns scion output into text lever
// shows (a value, a success result, an error) goes through it.
func (c *Client) filterOutput(output string) string {
	kept, dropped := splitSettingsWarnings(output)
	c.warnLog.record(dropped)
	return kept
}
