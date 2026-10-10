package voice

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortDir is a private temp dir with a short path under /tmp (a socket
// path is bounded, and TMPDIR can be long, as on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "vd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestListenDictateCreatesAPrivateSocket(t *testing.T) {
	base := shortDir(t)
	p := filepath.Join(base, "run", "d.sock")
	ln, err := ListenDictate(p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	di, _ := os.Lstat(filepath.Dir(p))
	si, _ := os.Lstat(p)
	if di.Mode().Perm() != 0o700 || si.Mode().Perm() != 0o600 || si.Mode()&os.ModeSocket == 0 {
		t.Fatalf("dir %v, socket %v", di.Mode(), si.Mode())
	}
	// In use: a second tool is refused, and the first keeps its socket.
	if _, err := ListenDictate(p); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second listener: %v", err)
	}
}

func TestListenDictateRemovesOnlyAStaleSocket(t *testing.T) {
	base := shortDir(t)
	p := filepath.Join(base, "d.sock")
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	// A killed tool leaves its socket behind.
	old, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	ln, err := ListenDictate(p)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ln.Close()
	// Anything but a socket is left alone.
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenDictate(p); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the file was removed")
	}
}

func TestListenDictateRefusesAnUnsafeDir(t *testing.T) {
	base := shortDir(t)
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenDictate(filepath.Join(open, "d.sock")); err == nil {
		t.Fatal("a directory other users can reach was accepted")
	}
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ListenDictate(filepath.Join(link, "d.sock")); err == nil {
		t.Fatal("a symlinked directory was accepted")
	}
	for _, bad := range []string{"rel/d.sock", "/a/../b.sock", "/" + strings.Repeat("x", MaxSocketPath)} {
		if _, err := ListenDictate(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// stubSvc is a Transcriber that answers what it is told.
type stubSvc struct {
	ready atomic.Bool
	text  string
	err   error
	calls atomic.Int32
}

func (s *stubSvc) Ready() bool { return s.ready.Load() }
func (s *stubSvc) Transcribe(context.Context, []byte) (string, error) {
	s.calls.Add(1)
	return s.text, s.err
}

// serveDictate serves h on a fresh socket until the test ends.
func serveDictate(t *testing.T, h http.Handler) string {
	t.Helper()
	p := filepath.Join(shortDir(t), "s", "d.sock")
	ln, err := ListenDictate(p)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewDictateServer(h)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return p
}

// The socket end to end: the tool's handler in front of the real Service
// running the fake whisper-server, and the proxy's client.
func TestDictateSocketWithFakeWhisper(t *testing.T) {
	bin := fakeWhisper(t)
	m := testModel("model")
	dir := t.TempDir()
	if err := os.WriteFile(ModelPath(dir, m), []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	var l logs
	svc := &Service{Model: m, ModelDir: dir, Server: bin, Port: freePort(t), Language: "en", Log: l.f,
		Supervisor: &Supervisor{ProbeEvery: 10 * time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	h := &DictateHandler{Svc: svc, Queue: &Queue{}, Model: m.Name, MaxSeconds: 2, Log: l.f}
	sock := serveDictate(t, h)
	c := &DictateClient{Socket: sock, HealthTTL: time.Millisecond}
	eventually(t, "available", c.Available)
	hl, err := c.Health(context.Background())
	if err != nil || !hl.Ready || hl.Model != m.Name || hl.MaxSeconds != 2 {
		t.Fatalf("%+v %v", hl, err)
	}
	text, err := c.Transcribe(context.Background(), CanonicalWAV(16000))
	if err != nil || !strings.Contains(text, "heard RIFF") || !strings.Contains(text, "language en") {
		t.Fatalf("%q %v", text, err)
	}
	// The tool checks the clip again: never sent on to whisper-server.
	for word, clip := range map[string][]byte{"bad-audio": []byte("RIFF not a wav at all, no"), "too-long": CanonicalWAV(3 * 16000)} {
		_, err := c.Transcribe(context.Background(), clip)
		if !errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), word) {
			t.Errorf("%s: %v", word, err)
		}
	}
	// No line of the tool's log carries the transcript.
	if l.count("heard") != 0 || l.count("transcribed in") != 1 {
		t.Fatalf("%q", l.lines)
	}
}

func TestDictateClientErrors(t *testing.T) {
	svc := &stubSvc{text: "hi"}
	h := &DictateHandler{Svc: svc, Queue: &Queue{}, Model: "m", MaxSeconds: 2}
	sock := serveDictate(t, h)
	c := &DictateClient{Socket: sock, HealthTTL: time.Hour}
	clip := CanonicalWAV(16000)
	// Not ready: the clip never reaches whisper-server.
	if c.Available() {
		t.Fatal("available while not ready")
	}
	if _, err := c.Transcribe(context.Background(), clip); !errors.Is(err, ErrNotSent) || !errors.Is(err, ErrNotReady) || svc.calls.Load() != 0 {
		t.Fatalf("%v %d", err, svc.calls.Load())
	}
	svc.ready.Store(true)
	if c.Available() {
		t.Fatal("the health answer was not cached")
	}
	if text, err := c.Transcribe(context.Background(), clip); err != nil || text != "hi" {
		t.Fatalf("%q %v", text, err)
	}
	// whisper-server could not be reached by the tool: not sent.
	svc.err = ErrNotSent
	if _, err := c.Transcribe(context.Background(), clip); !errors.Is(err, ErrNotSent) {
		t.Fatal(err)
	}
	// whisper-server had the clip and failed: sent, and the error is a
	// fixed word, never the server's text.
	svc.err = errors.New("secret transcript text")
	_, err := c.Transcribe(context.Background(), clip)
	if err == nil || errors.Is(err, ErrNotSent) || !errors.Is(err, ErrTool) || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	// No socket at all: not sent.
	gone := &DictateClient{Socket: filepath.Join(shortDir(t), "none.sock")}
	if _, err := gone.Transcribe(context.Background(), clip); !errors.Is(err, ErrNotSent) {
		t.Fatal(err)
	}
	if gone.Available() {
		t.Fatal("available without a socket")
	}
}

// The client sends nothing to a socket in a directory that is not a private
// directory of its user's: another user could have made it.
func TestDictateClientRefusesAnOpenDirectory(t *testing.T) {
	svc := &stubSvc{text: "hi"}
	svc.ready.Store(true)
	sock := serveDictate(t, &DictateHandler{Svc: svc, Queue: &Queue{}, MaxSeconds: 2})
	if err := os.Chmod(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &DictateClient{Socket: sock}
	_, err := c.Transcribe(context.Background(), CanonicalWAV(16000))
	if !errors.Is(err, ErrNotSent) || !strings.Contains(err.Error(), "private") || svc.calls.Load() != 0 {
		t.Fatalf("%v %d", err, svc.calls.Load())
	}
	if c.Available() {
		t.Fatal("available behind an open directory")
	}
}

func TestDictateHandlerRoutes(t *testing.T) {
	sock := serveDictate(t, &DictateHandler{Svc: &stubSvc{}, Queue: &Queue{}, MaxSeconds: 1})
	c := &DictateClient{Socket: sock}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/dictate", http.StatusMethodNotAllowed},
		{"POST", "/health", http.StatusMethodNotAllowed},
		{"GET", "/load", http.StatusNotFound},
		{"GET", "/inference", http.StatusNotFound},
	} {
		req, _ := http.NewRequest(tc.method, dictateBase+tc.path, nil)
		resp, err := c.http().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// blockSvc is a ready Transcriber whose Transcribe reports its start and
// then works until its context ends.
type blockSvc struct {
	started chan struct{}
	calls   atomic.Int32
}

func (s *blockSvc) Ready() bool { return true }
func (s *blockSvc) Transcribe(ctx context.Context, _ []byte) (string, error) {
	s.calls.Add(1)
	close(s.started)
	<-ctx.Done()
	return "", ctx.Err()
}

// A clip the caller gives up on while it waits in the tool's queue never
// reached whisper-server: not sent (the proxy gives its use back).
func TestDictateCanceledWhileQueuedIsNotSent(t *testing.T) {
	svc := &blockSvc{started: make(chan struct{})}
	q := &Queue{}
	var l logs
	sock := serveDictate(t, &DictateHandler{Svc: svc, Queue: q, MaxSeconds: 2, Log: l.f})
	// An agent clip holds the GPU.
	if err := q.Acquire(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	defer q.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := (&DictateClient{Socket: sock}).Transcribe(ctx, CanonicalWAV(16000))
	if !errors.Is(err, ErrNotSent) || svc.calls.Load() != 0 {
		t.Fatalf("%v, %d calls", err, svc.calls.Load())
	}
	eventually(t, "the tool logs the cancel", func() bool { return l.count("canceled while queued") == 1 })
	// A MaxWait that runs out in the queue is not sent either.
	_, err = (&DictateClient{Socket: sock, MaxWait: 100 * time.Millisecond}).Transcribe(context.Background(), CanonicalWAV(16000))
	if !errors.Is(err, ErrNotSent) || svc.calls.Load() != 0 {
		t.Fatalf("MaxWait in the queue: %v", err)
	}
}

// A clip whose transcription started counts, even when the caller gives up
// before it ends.
func TestDictateCanceledDuringTranscriptionIsSent(t *testing.T) {
	svc := &blockSvc{started: make(chan struct{})}
	var l logs
	sock := serveDictate(t, &DictateHandler{Svc: svc, Queue: &Queue{}, MaxSeconds: 2, Log: l.f})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-svc.started
		// Let the tool's 102 reach the client before it gives up.
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := (&DictateClient{Socket: sock}).Transcribe(ctx, CanonicalWAV(16000))
	if err == nil || errors.Is(err, ErrNotSent) || svc.calls.Load() != 1 {
		t.Fatalf("%v, %d calls", err, svc.calls.Load())
	}
	eventually(t, "the tool logs the cancel", func() bool { return l.count("canceled during transcription") == 1 })
}

// The tool's answer words: canceled (queued) is not sent; interrupted
// (during transcription) is sent.
func TestDictateClientCancelWords(t *testing.T) {
	for word, notSent := range map[string]bool{WordCanceled: true, WordInterrupted: false, WordFailed: false} {
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			answerJSON(w, http.StatusServiceUnavailable, map[string]string{"error": word})
		})
		sock := serveDictate(t, h)
		_, err := (&DictateClient{Socket: sock}).Transcribe(context.Background(), CanonicalWAV(16000))
		if err == nil || errors.Is(err, ErrNotSent) != notSent {
			t.Errorf("%s: %v", word, err)
		}
	}
}

// Available asks the tool once per HealthTTL however many callers there
// are: while one slow probe runs, the others keep the previous answer (or,
// before the first answer, wait for that probe).
func TestDictateAvailableSingleFlight(t *testing.T) {
	var probes atomic.Int32
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		<-release
		answerJSON(w, http.StatusOK, Health{Ready: true})
	})
	sock := serveDictate(t, h)
	c := &DictateClient{Socket: sock, HealthTTL: 50 * time.Millisecond}
	run := func(n int) []bool {
		out := make([]bool, n)
		done := make(chan int, n)
		for i := range n {
			go func() { out[i] = c.Available(); done <- i }()
		}
		for range n {
			<-done
		}
		return out
	}
	// First answer: everyone waits for one probe.
	go func() {
		for probes.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // the others pile in meanwhile
		release <- struct{}{}
	}()
	for i, ok := range run(20) {
		if !ok {
			t.Fatalf("caller %d: not available", i)
		}
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("%d probes, want 1", n)
	}
	// Stale: one caller probes (slowly); the others answer at once with
	// the previous value.
	time.Sleep(60 * time.Millisecond)
	first := make(chan bool)
	go func() { first <- c.Available() }()
	for probes.Load() != 2 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	for i, ok := range run(20) {
		if !ok {
			t.Fatalf("stale caller %d: not the previous answer", i)
		}
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("callers waited %v for the probe", d)
	}
	if n := probes.Load(); n != 2 {
		t.Fatalf("%d probes, want 2", n)
	}
	release <- struct{}{}
	if !<-first {
		t.Fatal("the probing caller")
	}
}
