package voice

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
)

// fakeWhisper builds tools/test/fakewhisper once per test binary.
func fakeWhisper(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is not on PATH; the fake whisper-server cannot be built")
	}
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakewhisper-")
		if err != nil {
			fakeErr = err
			return
		}
		fakeBin = filepath.Join(dir, "whisper-server")
		out, err := exec.Command(gobin, "build", "-o", fakeBin, "../../tools/test/fakewhisper").CombinedOutput()
		if err != nil {
			fakeErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakeBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fakeBin != "" {
		os.RemoveAll(filepath.Dir(fakeBin))
	}
	os.Exit(code)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// logs collects a supervisor's lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) f(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *logs) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func modelFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "model.bin")
	if err := os.WriteFile(p, []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSupervisorStartsServesAndStops(t *testing.T) {
	bin := fakeWhisper(t)
	port := freePort(t)
	var l logs
	s := &Supervisor{Program: bin, Args: ServerArgs(modelFile(t), port, false), Port: port, Log: l.f, ProbeEvery: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	eventually(t, "ready", s.Ready)
	text, err := Transcribe(ctx, &http.Client{}, s.Addr(), Request{WAV: []byte("RIFF....WAVE"), Language: "en", Prompt: "Lever"})
	if err != nil || !strings.Contains(text, "heard RIFF 12 bytes; language en; prompt Lever; format json; gpu false") {
		t.Fatalf("%q %v", text, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if s.Ready() || listening(port) {
		t.Fatal("the child is still serving after the stop")
	}
	if l.count("stopped") != 1 {
		t.Fatalf("%q", l.lines)
	}
}

func TestSupervisorRestartsWithBackoff(t *testing.T) {
	bin := fakeWhisper(t)
	port := freePort(t)
	var l logs
	s := &Supervisor{Program: bin, Args: append(ServerArgs(modelFile(t), port, true), "-exit-after", "200ms"), Port: port, Log: l.f,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, ProbeEvery: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	eventually(t, "three starts", func() bool { return l.count("started on") >= 3 })
	if l.count("exited after") < 2 || l.count("exit status 3") < 2 {
		t.Fatalf("%q", l.lines)
	}
	cancel()
	<-done
	if listening(port) {
		t.Fatal("the child outlived Run")
	}
}

func TestSupervisorLeavesATakenPortAlone(t *testing.T) {
	bin := fakeWhisper(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	var l logs
	s := &Supervisor{Program: bin, Args: ServerArgs(modelFile(t), port, false), Port: port, Log: l.f, MinBackoff: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	eventually(t, "the refusal", func() bool { return l.count("already in use") >= 2 })
	if s.Ready() || l.count("started on") != 0 {
		t.Fatalf("%v %q", s.Ready(), l.lines)
	}
	cancel()
	<-done
}

func TestChildEnvKeepsOnlyWhatAGPUBuildNeeds(t *testing.T) {
	got := ChildEnv([]string{"PATH=/bin", "HOME=/h", "CUDA_VISIBLE_DEVICES=0", "LD_LIBRARY_PATH=/l", "LC_ALL=C",
		"GITHUB_TOKEN=x", "LEVER_PUSH_TEST_HOSTS=y", "AWS_SECRET_ACCESS_KEY=z", "noequals"})
	want := []string{"PATH=/bin", "HOME=/h", "CUDA_VISIBLE_DEVICES=0", "LD_LIBRARY_PATH=/l", "LC_ALL=C"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestServiceRefusesAnUnverifiedModel(t *testing.T) {
	bin := fakeWhisper(t)
	m := testModel("m")
	m.SHA256 = Unverified
	var l logs
	s := &Service{Model: m, ModelDir: t.TempDir(), Server: bin, Port: freePort(t), Log: l.f}
	s.Run(context.Background()) // returns at once
	if s.Usable() || l.count("does not verify") != 1 || l.count("not verified yet") != 1 {
		t.Fatalf("%v %q", s.Usable(), l.lines)
	}
	if _, err := s.Transcribe(context.Background(), []byte("RIFF")); err != ErrNotReady {
		t.Fatal(err)
	}
}

func TestServiceRunsAVerifiedModel(t *testing.T) {
	bin := fakeWhisper(t)
	m := testModel("model")
	dir := t.TempDir()
	if err := os.WriteFile(ModelPath(dir, m), []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	var l logs
	s := &Service{Model: m, ModelDir: dir, Server: bin, Port: freePort(t), GPU: true, Prompt: "Fizzy", Log: l.f,
		Supervisor: &Supervisor{ProbeEvery: 10 * time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	eventually(t, "usable", s.Usable)
	eventually(t, "ready", func() bool { return s.Supervisor.Ready() })
	text, err := s.Transcribe(ctx, []byte("RIFFxxxx"))
	if err != nil || !strings.Contains(text, "language auto; prompt Fizzy; format json; gpu true") {
		t.Fatalf("%q %v", text, err)
	}
	cancel()
	<-done
	if s.Usable() {
		t.Fatal("still usable after the stop")
	}
	if _, err := s.Transcribe(context.Background(), []byte("RIFF")); err != ErrNotReady {
		t.Fatal(err)
	}
}
