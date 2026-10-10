package voice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentTree is a tree with the agent directory, and a transcriber over it.
func agentTree(t *testing.T) (string, *AgentTranscriber, *stubSvc) {
	t.Helper()
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, AgentDir), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := &stubSvc{text: " hello [BLANK_AUDIO] world "}
	svc.ready.Store(true)
	return tree, &AgentTranscriber{Tree: tree, MaxSeconds: 2, Svc: svc, Queue: &Queue{}}, svc
}

func put(t *testing.T, tree, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(tree, AgentDir, name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgentTranscribe(t *testing.T) {
	tree, a, _ := agentTree(t)
	put(t, tree, "note-1.wav", CanonicalWAV(16000))
	var lines []string
	a.Log = func(f string, args ...any) { lines = append(lines, f) }
	text, err := a.Transcribe(context.Background(), "manager", "note-1.wav")
	if err != nil || text != "hello world" {
		t.Fatalf("%q %v", text, err)
	}
	if len(lines) != 1 {
		t.Fatalf("%q", lines)
	}
}

func TestAgentFileNames(t *testing.T) {
	for _, ok := range []string{"a.wav", "note-1.wav", "_x.y.wav", "A9.wav"} {
		if err := CheckAgentFile(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".wav", ".hidden.wav", "../x.wav", "a/b.wav", "a.WAV", "a.mp3", "a.wav/", "-x\x00.wav", strings.Repeat("a", 94) + ".wav", "a b.wav"} {
		if err := CheckAgentFile(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	tree, a, svc := agentTree(t)
	put(t, tree, "ok.wav", CanonicalWAV(16000))
	if _, err := a.Transcribe(context.Background(), "m", "../whisper/ok.wav"); err == nil || svc.calls.Load() != 0 {
		t.Fatal("a path was accepted")
	}
}

func TestAgentRefusesLinks(t *testing.T) {
	tree, a, svc := agentTree(t)
	outside := filepath.Join(t.TempDir(), "secret.wav")
	if err := os.WriteFile(outside, CanonicalWAV(16000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, AgentDir, "link.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Transcribe(context.Background(), "m", "link.wav"); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("leaf symlink: %v", err)
	}
	if err := os.Link(outside, filepath.Join(tree, AgentDir, "hard.wav")); err == nil {
		if _, err := a.Transcribe(context.Background(), "m", "hard.wav"); err == nil || !strings.Contains(err.Error(), "link") {
			t.Fatalf("hard link: %v", err)
		}
	}
	// The directory itself a symlink to somewhere else.
	tree2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree2, ".lever-files"), 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "x.wav"), CanonicalWAV(16000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(tree2, AgentDir)); err != nil {
		t.Fatal(err)
	}
	a.Tree = tree2
	if _, err := a.Transcribe(context.Background(), "m", "x.wav"); err == nil {
		t.Fatal("a symlinked directory was followed")
	}
	if svc.calls.Load() != 0 {
		t.Fatal("a linked file reached the transcriber")
	}
	// Errors name no host path.
	_, err := a.Transcribe(context.Background(), "m", "missing.wav")
	if err == nil || strings.Contains(err.Error(), tree2) || strings.Contains(err.Error(), elsewhere) {
		t.Fatal(err)
	}
}

func TestAgentSizeAndFormat(t *testing.T) {
	tree, a, svc := agentTree(t)
	put(t, tree, "long.wav", CanonicalWAV(3*16000))
	if _, err := a.Transcribe(context.Background(), "m", "long.wav"); err == nil || !strings.Contains(err.Error(), "at most 2 seconds of audio") {
		t.Fatalf("too long: %v", err)
	}
	b := CanonicalWAV(16000)
	copy(b[36:], "LIST")
	put(t, tree, "list.wav", b)
	if _, err := a.Transcribe(context.Background(), "m", "list.wav"); err == nil || !strings.Contains(err.Error(), "canonical WAV") {
		t.Fatalf("bad format: %v", err)
	}
	if err := os.Mkdir(filepath.Join(tree, AgentDir, "dir.wav"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Transcribe(context.Background(), "m", "dir.wav"); err == nil {
		t.Fatal("a directory was read")
	}
	if svc.calls.Load() != 0 {
		t.Fatal("a refused file reached the transcriber")
	}
}

func TestAgentLimits(t *testing.T) {
	tree, a, svc := agentTree(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	a.MaxSeconds = 600
	put(t, tree, "short.wav", CanonicalWAV(1600))
	for i := 0; i < AgentClipsPerHour; i++ {
		if _, err := a.Transcribe(context.Background(), "w1", "short.wav"); err != nil {
			t.Fatalf("clip %d: %v", i, err)
		}
	}
	if _, err := a.Transcribe(context.Background(), "w1", "short.wav"); err == nil || !strings.Contains(err.Error(), "clips an hour") {
		t.Fatalf("over the hourly count: %v", err)
	}
	// Over a limit, the file is not even read.
	if _, err := a.Transcribe(context.Background(), "w1", "missing.wav"); err == nil || !strings.Contains(err.Error(), "clips an hour") {
		t.Fatalf("over the hourly count, a missing file: %v", err)
	}
	// Per caller: another agent is not affected.
	if _, err := a.Transcribe(context.Background(), "w2", "short.wav"); err != nil {
		t.Fatal(err)
	}
	// An hour on, the clips count again; the daily audio cap still holds.
	now = now.Add(time.Hour)
	put(t, tree, "ten.wav", CanonicalWAV(600*16000))
	for i := 0; i < 5; i++ {
		if _, err := a.Transcribe(context.Background(), "w2", "ten.wav"); err != nil {
			t.Fatalf("long clip %d: %v", i, err)
		}
	}
	if _, err := a.Transcribe(context.Background(), "w2", "ten.wav"); err == nil || !strings.Contains(err.Error(), "minutes of audio a day") {
		t.Fatalf("over the daily audio: %v", err)
	}
	// A clip that never reached whisper-server is given back.
	svc.ready.Store(false)
	for i := 0; i < AgentClipsPerHour+5; i++ {
		if _, err := a.Transcribe(context.Background(), "w3", "short.wav"); err == nil || !strings.Contains(err.Error(), "not ready") {
			t.Fatalf("not ready: %v", err)
		}
	}
	svc.ready.Store(true)
	svc.err = ErrNotSent
	if _, err := a.Transcribe(context.Background(), "w3", "short.wav"); err == nil {
		t.Fatal("no error")
	}
	svc.err = nil
	if _, err := a.Transcribe(context.Background(), "w3", "short.wav"); err != nil {
		t.Fatalf("unsent clips were counted: %v", err)
	}
}

// An agent clip waits behind the GPU and gives up after QueueWait, given
// back.
func TestAgentQueueWait(t *testing.T) {
	tree, a, svc := agentTree(t)
	put(t, tree, "a.wav", CanonicalWAV(16000))
	a.QueueWait = 20 * time.Millisecond
	_ = a.Queue.Acquire(context.Background(), true)
	if _, err := a.Transcribe(context.Background(), "m", "a.wav"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal(err)
	}
	a.Queue.Release()
	if svc.calls.Load() != 0 {
		t.Fatal("ran without the GPU")
	}
}

// An agent clip holds the GPU at most about as long as the longest agent
// clip: dictation waits behind it no longer.
func TestAgentMaxWait(t *testing.T) {
	if AgentMaxWait(120, true) != 120*time.Second || AgentMaxWait(5, true) != 30*time.Second {
		t.Fatal(AgentMaxWait(120, true), AgentMaxWait(5, true))
	}
	if AgentMaxWait(120, false) != 480*time.Second || AgentMaxWait(5, false) != 2*time.Minute {
		t.Fatal(AgentMaxWait(120, false), AgentMaxWait(5, false))
	}
	if AgentMaxSeconds(0, 300) != DefaultAgentMaxSeconds || AgentMaxSeconds(0, 60) != 60 || AgentMaxSeconds(30, 300) != 30 {
		t.Fatal("AgentMaxSeconds")
	}
	if CheckAgentMaxSeconds(0, 300) == nil || CheckAgentMaxSeconds(301, 300) == nil || CheckAgentMaxSeconds(300, 300) != nil {
		t.Fatal("CheckAgentMaxSeconds")
	}
}
