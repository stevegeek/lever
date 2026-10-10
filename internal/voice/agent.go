package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/lever/internal/fsutil"
)

// AgentDir is the tree-relative directory the clips of the agent operation
// are read from (/workspace/.lever-files/whisper in the manager's
// container). It is at the tree root, so only the manager can write it (a
// worker mounts only its own dir); but it is read for every caller, so any
// agent with a transcribe capability (a delegated or obtained one included)
// reads the files the manager put there. Like the github tool's bundle
// directory, it does not depend on the caller.
const AgentDir = ".lever-files/whisper"

// The per-caller limits of the agent operation, in memory (they start again
// when the tool restarts).
const (
	AgentClipsPerHour  = 30
	AgentSecondsPerDay = 60 * 60
)

var agentFileRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,92}\.wav$`)

// CheckAgentFile accepts one plain file name ending in .wav.
func CheckAgentFile(name string) error {
	if !agentFileRE.MatchString(name) {
		return fmt.Errorf("file %q: want a single file name like note-1.wav in /workspace/%s", name, AgentDir)
	}
	return nil
}

// AgentTranscriber is the agent operation transcribe: a WAV file the agent
// wrote into AgentDir, read with no symbolic link on any component and no
// hard link, checked like a dictation clip, and transcribed after any
// waiting dictation.
type AgentTranscriber struct {
	Tree string
	// MaxSeconds is the longest agent clip (-agent-max-seconds).
	MaxSeconds int
	Svc        Transcriber
	Queue      *Queue
	// QueueWait bounds the wait for the GPU; zero = 2 minutes.
	QueueWait time.Duration
	// MaxWait bounds one transcription once it has the GPU; zero =
	// AgentMaxWait(MaxSeconds). An agent's call cannot be cancelled from its
	// side (the handler has no request context), and a running clip is
	// never interrupted for dictation, so this also bounds how long an agent
	// clip can keep a dictation clip waiting.
	MaxWait time.Duration
	// Log receives one line per call: the caller, the file name, the
	// clip's length, the outcome and the latency; never the text.
	Log func(format string, a ...any)
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time

	mu   sync.Mutex
	used map[string][]agentUse
}

type agentUse struct {
	at      time.Time
	seconds float64
}

func (a *AgentTranscriber) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *AgentTranscriber) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log(format, args...)
	}
}

// Transcribe reads, checks and transcribes caller's file and returns the
// cleaned transcript (CleanText). Its errors are fit for the agent: they
// name no host path.
func (a *AgentTranscriber) Transcribe(ctx context.Context, caller, file string) (text string, err error) {
	seconds := 0.0
	start := time.Now()
	defer func() {
		outcome := "ok"
		if err != nil {
			outcome = "refused: " + err.Error()
		}
		a.logf("voice: transcribe caller=%q file=%q audio_seconds=%.1f latency_ms=%d %s", caller, file, seconds, time.Since(start).Milliseconds(), outcome)
	}()
	if err := CheckAgentFile(file); err != nil {
		return "", err
	}
	// Over a limit already: refused before the file is read.
	if err := a.check(caller, 0, a.now(), false); err != nil {
		return "", err
	}
	clip, err := a.read(file)
	if err != nil {
		return "", err
	}
	seconds, word := CheckWAV(clip, a.MaxSeconds)
	switch word {
	case WordTooLong:
		return "", a.tooLong(file)
	case WordBadAudio:
		return "", fmt.Errorf("file %s is not a canonical WAV of PCM s16le, 16 kHz, mono with a 44-byte header and at least 0.1 s of audio", file)
	}
	at := a.now()
	if err := a.check(caller, seconds, at, true); err != nil {
		return "", err
	}
	if !a.Svc.Ready() {
		a.refund(caller, seconds, at)
		return "", errors.New("whisper-server is not ready; try again later")
	}
	wait := a.QueueWait
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	qctx, cancel := context.WithTimeout(ctx, wait)
	err = a.Queue.Acquire(qctx, false)
	cancel()
	if err != nil {
		a.refund(caller, seconds, at)
		return "", errors.New("the transcriber is busy; try again later")
	}
	maxWait := a.MaxWait
	if maxWait <= 0 {
		maxWait = AgentMaxWait(a.MaxSeconds)
	}
	tctx, tcancel := context.WithTimeout(ctx, maxWait)
	raw, err := a.Svc.Transcribe(tctx, clip)
	tcancel()
	a.Queue.Release()
	if err != nil {
		// Only a clip that never reached whisper-server is given back.
		if errors.Is(err, ErrNotSent) {
			a.refund(caller, seconds, at)
			return "", errors.New("whisper-server is not ready; try again later")
		}
		return "", errors.New("the transcription failed")
	}
	return CleanText(raw), nil
}

// read copies the file into memory: no symbolic link on any component, no
// hard link, at most the largest clip of MaxSeconds.
func (a *AgentTranscriber) read(file string) ([]byte, error) {
	limit := MaxClipBytes(a.MaxSeconds)
	f, _, err := fsutil.OpenInTreeNoLinks(a.Tree, path.Join(AgentDir, file), limit)
	switch {
	case errors.Is(err, fsutil.ErrSymlink) || errors.Is(err, fsutil.ErrHardLink):
		return nil, fmt.Errorf("file %s: refused (a symbolic or hard link)", file)
	case errors.Is(err, fsutil.ErrFileTooLarge):
		return nil, a.tooLong(file)
	case errors.Is(err, fsutil.ErrNotRegularFile):
		return nil, fmt.Errorf("file %s is not a regular file", file)
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("file %s: no such file in /workspace/%s", file, AgentDir)
	case err != nil:
		return nil, fmt.Errorf("file %s: cannot be read", file)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("file %s: cannot be read", file)
	}
	if int64(len(b)) > limit {
		return nil, a.tooLong(file)
	}
	return b, nil
}

// tooLong is the refusal of a file over MaxSeconds.
func (a *AgentTranscriber) tooLong(file string) error {
	return fmt.Errorf("file %s is too long: the agent operation takes at most %d seconds of audio (the tool's -agent-max-seconds); split it into shorter clips", file, a.MaxSeconds)
}

// AgentMaxWait is how long one agent clip of at most maxSeconds may keep
// the GPU: as long as the clip, at least 30 s. A GPU transcribes far faster
// than real time; a clip that takes longer is cut off as failed.
func AgentMaxWait(maxSeconds int) time.Duration {
	return time.Duration(max(maxSeconds, 30)) * time.Second
}

// check refuses one more clip of seconds by caller over AgentClipsPerHour
// or AgentSecondsPerDay, and with take also counts it. Uses older than a
// day are forgotten.
func (a *AgentTranscriber) check(caller string, seconds float64, now time.Time, take bool) error {
	key := strings.ToLower(caller)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.used == nil {
		a.used = map[string][]agentUse{}
	}
	for k, us := range a.used {
		keep := us[:0]
		for _, u := range us {
			if now.Sub(u.at) < 24*time.Hour {
				keep = append(keep, u)
			}
		}
		if len(keep) == 0 {
			delete(a.used, k)
		} else {
			a.used[k] = keep
		}
	}
	n, total := 0, seconds
	for _, u := range a.used[key] {
		if now.Sub(u.at) < time.Hour {
			n++
		}
		total += u.seconds
	}
	switch {
	case n >= AgentClipsPerHour:
		return fmt.Errorf("limit: %d clips an hour; try again later", AgentClipsPerHour)
	case total > AgentSecondsPerDay:
		return fmt.Errorf("limit: %d minutes of audio a day; try again later", AgentSecondsPerDay/60)
	}
	if take {
		a.used[key] = append(a.used[key], agentUse{at: now, seconds: seconds})
	}
	return nil
}

// refund gives back the use take recorded at the same moment for the same
// length.
func (a *AgentTranscriber) refund(caller string, seconds float64, at time.Time) {
	key := strings.ToLower(caller)
	a.mu.Lock()
	defer a.mu.Unlock()
	us := a.used[key]
	for i := len(us) - 1; i >= 0; i-- {
		if us[i].at.Equal(at) && us[i].seconds == seconds {
			a.used[key] = append(us[:i], us[i+1:]...)
			return
		}
	}
}
