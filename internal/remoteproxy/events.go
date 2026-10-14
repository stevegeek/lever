package remoteproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
)

// A contact's events stream. The hub's chat events carry message text and
// sender names, but the page uses an event only as "reload" and reads only
// its subject. So each event is reduced to its subject and numeric id: no
// text, sender or message id reaches the contact. Anything unreadable is
// dropped. Each event is returned as soon as it is complete: ReverseProxy
// flushes every write of a text/event-stream answer.

const (
	maxEventLine  = 1 << 20
	maxEventBytes = 2 << 20
)

var (
	eventIDRE   = regexp.MustCompile(`^[0-9]{1,20}$`)
	heartbeatRE = regexp.MustCompile(`^:heartbeat [0-9]{1,20}$`)
	subjectRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
)

// reduceEvents: an answer that is not a plain event stream becomes empty.
func reduceEvents(uid string) func(*http.Response) {
	return func(resp *http.Response) {
		if resp.StatusCode != http.StatusOK {
			return
		}
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if ct != "text/event-stream" || resp.Header.Get("Content-Encoding") != "" {
			_ = resp.Body.Close()
			setBody(resp, nil)
			return
		}
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Body = &eventReducer{src: bufio.NewReaderSize(resp.Body, 64<<10), body: resp.Body, prefix: "user." + uid + "."}
	}
}

type eventReducer struct {
	src    *bufio.Reader
	body   io.Closer
	prefix string
	out    bytes.Buffer
	err    error
}

func (e *eventReducer) Close() error { return e.body.Close() }

func (e *eventReducer) Read(p []byte) (int, error) {
	for e.out.Len() == 0 && e.err == nil {
		lines, err := e.nextEvent()
		if lines != nil {
			e.out.WriteString(e.reduce(lines))
		}
		e.err = err
	}
	if e.out.Len() > 0 {
		return e.out.Read(p)
	}
	return 0, e.err
}

// nextEvent drops (returns nil for) an event over the line or size bounds.
func (e *eventReducer) nextEvent() ([]string, error) {
	var lines []string
	size, drop := 0, false
	for {
		line, tooLong, err := e.readLine()
		if err != nil {
			return nil, err // a partial event at the end is dropped
		}
		if line == "" && !tooLong {
			if drop || len(lines) == 0 {
				return nil, nil
			}
			return lines, nil
		}
		size += len(line)
		if tooLong || size > maxEventBytes {
			drop, lines = true, nil
			continue
		}
		if !drop {
			lines = append(lines, line)
		}
	}
}

// readLine keeps no content of a line over maxEventLine.
func (e *eventReducer) readLine() (string, bool, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := e.src.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > maxEventLine {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return "", tooLong, err
		}
		return strings.TrimRight(string(buf), "\r\n"), tooLong, nil
	}
}

// reduce passes heartbeats (the comment and the named event) and the
// reconnect hint as fixed text, and keeps an update's numeric id and subject
// with empty data; it drops the rest.
func (e *eventReducer) reduce(lines []string) string {
	var id, event, data string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, ":"):
			if len(lines) == 1 && heartbeatRE.MatchString(l) {
				return l + "\n\n"
			}
			return ""
		case strings.HasPrefix(l, "id: "):
			id = strings.TrimPrefix(l, "id: ")
		case strings.HasPrefix(l, "event: "):
			event = strings.TrimPrefix(l, "event: ")
		case strings.HasPrefix(l, "data: "):
			if data != "" {
				return "" // the hub sends one data line; more is not ours to read
			}
			data = strings.TrimPrefix(l, "data: ")
		default:
			return ""
		}
	}
	switch event {
	case "reconnect":
		return "event: reconnect\ndata: {}\n\n"
	case "heartbeat":
		// The hub's web client counts a stream silent for over a minute as
		// dead, and only a named event reaches it. Its data is a millisecond
		// time, the same digits as an id.
		if id == "" && eventIDRE.MatchString(data) {
			return "event: heartbeat\ndata: " + data + "\n\n"
		}
		return ""
	}
	if event != "update" {
		return ""
	}
	var in struct {
		Subject string `json:"subject"`
	}
	if json.Unmarshal([]byte(data), &in) != nil || !subjectRE.MatchString(in.Subject) || !strings.HasPrefix(in.Subject, e.prefix) {
		return ""
	}
	b, _ := json.Marshal(map[string]any{"subject": in.Subject, "data": struct{}{}})
	var sb strings.Builder
	if eventIDRE.MatchString(id) {
		sb.WriteString("id: " + id + "\n")
	}
	sb.WriteString("event: update\ndata: " + string(b) + "\n\n")
	return sb.String()
}
