package cli

import (
	"io"
	"os"
	"strings"
)

// readFileTail returns up to the last max bytes of the file at path.
func readFileTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if off := fi.Size() - max; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}

// lastLine returns the last non-empty line of s, or "" when there is none.
// Surrounding whitespace on that line is preserved; only blank lines are
// skipped.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return lines[i]
		}
	}
	return ""
}

// lastFileLine returns the final non-empty line within the last tail bytes of
// the file at path, trimmed, or "" when the file is unreadable or blank. Only
// the tail is read so a long-lived log never costs a full read.
func lastFileLine(path string, tail int64) string {
	data, err := readFileTail(path, tail)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(lastLine(string(data)))
}
