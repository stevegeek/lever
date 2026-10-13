package scion

import (
	"regexp"
	"strings"
	"sync"
)

// settingsKeysWarnRE matches, as a whole line, the warning scion's settings
// loader prints on every CLI call against a v1 settings file:
//
//	2026/09/22 21:39:27 WARN settings file contains unrecognized keys (these will be ignored) keys="[hub.project_id runtimes[*].type]" path=...
//
// It is harmless: scion's legacy-format probe complains about keys the
// versioned loader then reads. Only this exact message with the "settings"
// label is matched, so any other scion warning, including the same message
// for a different file, still reaches the operator.
var settingsKeysWarnRE = regexp.MustCompile(
	`^(?:\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} )?WARN settings file contains unrecognized keys \(these will be ignored\)(?: [a-z_]+=.*)?$`)

// IsSettingsKeysWarning reports whether line is scion's benign
// unrecognized-settings-keys warning (see settingsKeysWarnRE). Surrounding
// whitespace and terminal colour codes are ignored.
func IsSettingsKeysWarning(line string) bool {
	return settingsKeysWarnRE.MatchString(strings.TrimSpace(ansiRE.ReplaceAllString(line, "")))
}

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

// warnLog records filtered settings warnings once per process, compared
// without their timestamp: scion prints the same warning on every call.
type warnLog struct {
	sink func(line string)
}

var warnSeen = struct {
	mu   sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

var lstdTimestampRE = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

func (w *warnLog) record(lines []string) {
	if w == nil || w.sink == nil || len(lines) == 0 {
		return
	}
	warnSeen.mu.Lock()
	defer warnSeen.mu.Unlock()
	for _, l := range lines {
		key := lstdTimestampRE.ReplaceAllString(l, "")
		if warnSeen.seen[key] {
			continue
		}
		warnSeen.seen[key] = true
		w.sink(l)
	}
}

// filterOutput drops the settings warnings from raw scion output and hands
// them to the log sink. Every path that shows scion output must go through it.
func (c *Client) filterOutput(output string) string {
	kept, dropped := splitSettingsWarnings(output)
	c.warnLog.record(dropped)
	return kept
}
