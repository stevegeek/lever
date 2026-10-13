package lima

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (l *Lima) instanceDir(inst realizedInstance) string {
	if inst.Dir != "" {
		return inst.Dir
	}
	home := os.Getenv("LIMA_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".lima")
	}
	return filepath.Join(home, l.Machine())
}

const tailRead = 64 << 10

// lastLines returns up to n last lines of the file at path; nil when it
// cannot be read.
func lastLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > tailRead {
		f.Seek(-tailRead, io.SeekEnd)
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if l := limaLine(sc.Text()); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// limaLine: Lima writes logrus JSON lines; anything else is kept as is.
func limaLine(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return raw
	}
	var v struct {
		Level string `json:"level"`
		Msg   string `json:"msg"`
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil || v.Msg == "" {
		return raw
	}
	out := v.Msg
	if v.Error != "" {
		out += ": " + v.Error
	}
	if v.Level != "" && v.Level != "info" {
		out = v.Level + ": " + out
	}
	return out
}

// watchLimaStage: Lima appends to ha.stderr.log across starts, so only lines
// written after the watch began count. The returned func stops it.
func watchLimaStage(ctx context.Context, path string, detail func(string)) func() {
	var offset int64
	if fi, err := os.Stat(path); err == nil {
		offset = fi.Size()
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if msg, next := latestLimaMsg(path, offset); msg != "" {
					offset = next
					detail(msg)
				}
			}
		}
	}()
	return func() { close(stop); <-done }
}

// latestLimaMsg reads a file that shrank (rotated) from its start.
func latestLimaMsg(path string, offset int64) (string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return "", offset
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", offset
	}
	if fi.Size() < offset {
		offset = 0
	}
	if fi.Size()-offset > tailRead {
		offset = fi.Size() - tailRead
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", offset
	}
	b, err := io.ReadAll(io.LimitReader(f, tailRead))
	if err != nil {
		return "", offset
	}
	// Only whole lines: a partial last line is read again next time.
	end := strings.LastIndexByte(string(b), '\n')
	if end < 0 {
		return "", offset
	}
	var last string
	for _, l := range strings.Split(string(b[:end]), "\n") {
		if m := limaLine(l); m != "" {
			last = m
		}
	}
	return last, offset + int64(end) + 1
}
