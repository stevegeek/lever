package servicehost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// maxStdioLine bounds one message from a stdio server. A longer line is a
// broken or hostile server and ends the process.
const maxStdioLine = 16 << 20

// initTimeout bounds a stdio server's answer to initialize.
const initTimeout = 30 * time.Second

// stopGrace is how long a stdio server gets to exit after its stdin closes.
const stopGrace = 2 * time.Second

// baseEnv is all a stdio server inherits by default, so one tool's
// credentials in the service host's environment never reach another.
var baseEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR"}

var (
	errProcExited = errors.New("tool process exited")
	errTimeout    = errors.New("call timed out")
)

// stdioServer runs one stdio MCP server, starting it on first use and again
// after it exits. The service host initializes it itself and keeps no MCP
// session, so a client never sees the restart.
type stdioServer struct {
	tool    Tool
	defDir  string
	log     *slog.Logger
	version string

	mu     sync.Mutex
	proc   *stdioProc
	closed bool
	// failedAt is when the last start or initialize failed: a server that
	// cannot start is not respawned on every call.
	failedAt time.Time
}

// restartBackoff is how long after a failed start calls fail fast.
const restartBackoff = 2 * time.Second

type stdioProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex

	nextID  atomic.Int64
	pendMu  sync.Mutex
	pending map[int64]chan []byte

	// done closes when the server's stdout ends or it exits, exited once
	// it is reaped.
	done       chan struct{}
	doneOnce   sync.Once
	exited     chan struct{}
	initResult json.RawMessage
}

func (s *stdioServer) env() ([]string, error) {
	var env []string
	for _, k := range baseEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, k := range s.tool.PassEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range s.tool.Env {
		env = append(env, k+"="+v)
	}
	for k, p := range s.tool.EnvFiles {
		v, err := readSecretValue(p, true)
		if err != nil {
			return nil, fmt.Errorf("env_files %s: %w", k, err)
		}
		env = append(env, k+"="+v)
	}
	return env, nil
}

// running returns the live process, starting and initializing one if there is
// none. protocolVersion is what the child is initialized with when it starts.
func (s *stdioServer) running(ctx context.Context, protocolVersion string) (*stdioProc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errProcExited
	}
	if p := s.proc; p != nil {
		select {
		case <-p.done:
		default:
			return p, nil
		}
	}
	if !s.failedAt.IsZero() && time.Since(s.failedAt) < restartBackoff {
		return nil, errProcExited
	}
	p, err := s.start()
	if err != nil {
		s.failedAt = time.Now()
		s.log.Warn("servicehost.stdio_start_failed", "tool", s.tool.Name, "err", err.Error())
		return nil, err
	}
	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if err := p.initialize(ictx, protocolVersion, s.version); err != nil {
		go p.stop()
		s.failedAt = time.Now()
		s.log.Warn("servicehost.stdio_init_failed", "tool", s.tool.Name, "err", err.Error())
		return nil, err
	}
	s.proc, s.failedAt = p, time.Time{}
	return p, nil
}

func (s *stdioServer) start() (*stdioProc, error) {
	env, err := s.env()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(s.tool.Command[0], s.tool.Command[1:]...)
	cmd.Env = env
	// Its own process group, so stopping it also stops what it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = s.defDir
	if s.tool.Dir != "" {
		cmd.Dir = s.tool.Dir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.tool.Command[0], err)
	}
	s.log.Info("servicehost.stdio_started", "tool", s.tool.Name, "pid", cmd.Process.Pid)
	p := &stdioProc{cmd: cmd, stdin: stdin, pending: map[int64]chan []byte{},
		done: make(chan struct{}), exited: make(chan struct{})}
	go s.logStderr(stderr)
	go func() {
		p.read(stdout)
		p.markDone()
		p.stop()
	}()
	// Reaped as soon as it exits, even while something it started still
	// holds its stdout; Wait then closes the read end.
	go func() {
		err := cmd.Wait()
		p.markDone()
		close(p.exited)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		s.log.Warn("servicehost.stdio_exited", "tool", s.tool.Name, "pid", cmd.Process.Pid, "err", fmt.Sprint(err))
	}()
	return p, nil
}

// logStderr keeps a stdio server's diagnostics, one bounded line each. They
// are the server's own words, so they go to the log only.
func (s *stdioServer) logStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		if len(line) > 1024 {
			line = line[:1024]
		}
		s.log.Info("servicehost.stdio_stderr", "tool", s.tool.Name, "line", line)
	}
	_, _ = io.Copy(io.Discard, r)
}

func (s *stdioServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.proc != nil {
		s.proc.stop()
	}
}

func (p *stdioProc) markDone() { p.doneOnce.Do(func() { close(p.done) }) }

// stop closes stdin, which ends a well-behaved server, and kills its process
// group after stopGrace. It returns once the server is reaped.
func (p *stdioProc) stop() {
	p.writeMu.Lock()
	_ = p.stdin.Close()
	p.writeMu.Unlock()
	select {
	case <-p.exited:
	case <-time.After(stopGrace):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.exited
	}
}

// read routes each response to its waiting call. Requests from the server
// (sampling, roots) get "method not found", and notifications are dropped:
// the service host is the server's only client and offers neither.
func (p *stdioProc) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br, maxStdioLine)
		if err != nil {
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		rawID, hasID := m["id"]
		if _, isReq := m["method"]; isReq {
			if hasID {
				p.write(fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, rawID))
			}
			continue
		}
		if !hasID {
			continue
		}
		id, err := strconv.ParseInt(string(rawID), 10, 64)
		if err != nil {
			continue
		}
		p.pendMu.Lock()
		ch, ok := p.pending[id]
		delete(p.pending, id)
		p.pendMu.Unlock()
		if ok {
			ch <- line
		}
	}
}

// readLine reads one newline-terminated line of at most max bytes.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > max {
			return nil, errors.New("line too long")
		}
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

func (p *stdioProc) write(b []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		return errProcExited
	}
	return nil
}

// roundTrip sends msg under a fresh id and returns the server's response
// with the caller's id put back.
func (p *stdioProc) roundTrip(ctx context.Context, msg map[string]any) ([]byte, error) {
	callerID, hasID := msg["id"]
	id := p.nextID.Add(1)
	out := make(map[string]any, len(msg))
	for k, v := range msg {
		out[k] = v
	}
	out["id"] = id
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	ch := make(chan []byte, 1)
	p.pendMu.Lock()
	p.pending[id] = ch
	p.pendMu.Unlock()
	defer func() {
		p.pendMu.Lock()
		delete(p.pending, id)
		p.pendMu.Unlock()
	}()
	if err := p.write(b); err != nil {
		return nil, err
	}
	select {
	case line := <-ch:
		var resp map[string]json.RawMessage
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, err
		}
		if !hasID {
			callerID = nil
		}
		rawCaller, err := json.Marshal(callerID)
		if err != nil {
			return nil, err
		}
		resp["id"] = rawCaller
		return json.Marshal(resp)
	case <-p.done:
		return nil, errProcExited
	case <-ctx.Done():
		return nil, errTimeout
	}
}

func (p *stdioProc) initialize(ctx context.Context, protocolVersion, version string) error {
	resp, err := p.roundTrip(ctx, map[string]any{
		"jsonrpc": "2.0", "id": 0, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "lever-service-host", "version": version},
		},
	})
	if err != nil {
		return err
	}
	var m struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(resp, &m); err != nil {
		return err
	}
	if len(m.Result) == 0 || string(m.Result) == "null" {
		return fmt.Errorf("initialize refused: %.200s", m.Error)
	}
	p.initResult = m.Result
	return p.write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
}
