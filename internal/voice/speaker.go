package voice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrSpeakNotSent: the text never reached the speech engine (the program or
// the model failed its check, or the text or voice was refused first). No
// work was done, so its use of the limits is given back.
var ErrSpeakNotSent = errors.New("the text did not reach the speech engine")

// ErrSpeakFailed: the engine ran and gave no usable audio.
var ErrSpeakFailed = errors.New("the speech engine failed")

// Speaker is the speech engine as lever-tool-tts runs it: one run of
// sherpa-onnx-offline-tts per text (sherpa.go), as the tool's child, with
// the program and the model checked before every run. There is no
// long-running engine and no port: nothing listens but the tool's own
// socket.
type Speaker struct {
	Model    SpeechModel
	ModelDir string // -models: the model's files are in SpeechDir(ModelDir, Model)
	Server   string // -server: sherpa-onnx-offline-tts
	Voice    string // -voice: the default voice
	Speed    float64
	Threads  int
	MaxChars int
	// MaxWait bounds one run; zero = 2 minutes.
	MaxWait time.Duration
	// Log receives lever's own lines; nil drops them.
	Log func(format string, a ...any)

	once sync.Once
	run  chan struct{} // one run at a time

	mu      sync.Mutex
	real    string // the program's real path, pinned at its first good check
	stamp   string // speechStamp of the files at their last full verify that passed
	bad     string // speechStamp of the files at their last full verify that failed
	badErr  error
	checked time.Time
}

func (s *Speaker) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// Check runs before every run: the program must pass CheckServer and still
// resolve to the real file it resolved to the first time, and the model's
// files must verify. A full verify (every file hashed) runs only when the
// files' stamp changed since the last one; a stamp that failed before
// fails again without a hash. It returns the program's real path.
func (s *Speaker) Check() (string, error) {
	if why := s.Model.Pinned(); why != "" {
		return "", fmt.Errorf("%w: %s", ErrUnpinned, why)
	}
	real, err := CheckServer(s.Server)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.real == "" {
		s.real = real
	}
	if real != s.real {
		return "", fmt.Errorf("voice: -server now resolves to %s, not %s; restart the tool (lever reload) to accept it", real, s.real)
	}
	before := speechStamp(s.ModelDir, s.Model)
	switch {
	case before != "" && before == s.stamp:
		return real, nil
	case before != "" && before == s.bad:
		return "", s.badErr
	}
	err = VerifySpeech(s.ModelDir, s.Model)
	after := speechStamp(s.ModelDir, s.Model)
	if err == nil && before != after {
		err = fmt.Errorf("%w: the files of %s changed while they were checked", ErrModel, s.Model.Name)
	}
	if err != nil {
		if speechMissing(err) {
			err = fmt.Errorf("the speech model %s is not downloaded into %s (run `lever voice fetch %s`)", s.Model.Name, s.ModelDir, s.Model.Name)
		} else {
			err = fmt.Errorf("the speech model %s does not verify: %w (run `lever voice fetch %s`)", s.Model.Name, err, s.Model.Name)
		}
		s.stamp, s.bad, s.badErr = "", after, err
		return "", err
	}
	s.stamp, s.bad, s.badErr = after, "", nil
	return real, nil
}

// Ready reports whether a run could start now (Check passes).
func (s *Speaker) Ready() bool {
	_, err := s.Check()
	return err == nil
}

// Speak turns text into a WAV (16-bit PCM, mono, at the model's rate) with
// voice ("" = the default). The text is cleaned and bounded first
// (SpeechInput). One run at a time; a caller waits for its turn until ctx
// ends. The error never carries the text.
func (s *Speaker) Speak(ctx context.Context, text, voice string) ([]byte, error) {
	s.once.Do(func() { s.run = make(chan struct{}, 1) })
	if voice == "" {
		voice = s.Voice
	}
	v, ok := s.Model.Voice(voice)
	if !ok {
		return nil, fmt.Errorf("%w: the model %s has no voice %q", ErrSpeakNotSent, s.Model.Name, voice)
	}
	in, err := SpeechInput(text, s.MaxChars)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSpeakNotSent, err)
	}
	select {
	case s.run <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrSpeakNotSent, ctx.Err())
	}
	defer func() { <-s.run }()
	real, err := s.Check()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSpeakNotSent, err)
	}
	wait := s.MaxWait
	if wait <= 0 {
		wait = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tmp, err := os.MkdirTemp(s.ModelDir, speakTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("%w: a private directory for the output: %v", ErrSpeakNotSent, err)
	}
	// The audio is on disk only while the engine writes it, in a 0700
	// directory of the tool's: read, then removed.
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "speech.wav")
	args := SherpaArgs(s.Model, s.ModelDir, v, s.Speed, s.Threads, out, in)
	if err := runEngine(ctx, real, tmp, args); err != nil {
		return nil, err
	}
	f, err := os.Open(out)
	if err != nil {
		return nil, fmt.Errorf("%w: no output file", ErrSpeakFailed)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSpeechFile(s.Model.SampleRate)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading the output: %v", ErrSpeakFailed, err)
	}
	if int64(len(raw)) > maxSpeechFile(s.Model.SampleRate) {
		return nil, fmt.Errorf("%w: the output is too large", ErrSpeakFailed)
	}
	wav, err := SpeechWAV(raw, s.Model.SampleRate)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSpeakFailed, err)
	}
	return wav, nil
}

// speakTempPrefix names the per-run output directories in -models.
const speakTempPrefix = ".speak-"

// RemoveStaleSpeech deletes the per-run output directories a killed tool
// left in dir: those untouched for longer than any run may take (another
// tool sharing the directory may be running one now). Best effort; the
// tool runs it at start.
func RemoveStaleSpeech(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), speakTempPrefix) || !e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > staleSpeechAfter {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// staleSpeechAfter is well past the longest run (Speaker.MaxWait's default).
const staleSpeechAfter = 10 * time.Minute

// runEngine runs the program once: no stdin, stdout and stderr discarded
// (the program prints the text, assumption 5), a reduced environment
// (ChildEnv), dir as its working directory, its own process group, and
// Pdeathsig on Linux. When ctx ends first the group is killed.
func runEngine(ctx context.Context, program, dir string, args []string) error {
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	cmd.Env = ChildEnv(os.Environ())
	cmd.SysProcAttr = childSysProcAttr()
	// Pdeathsig is tied to the thread that started the child: keep it until
	// the child is gone.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: starting the engine: %v", ErrSpeakNotSent, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %v", ErrSpeakFailed, exitText(err))
		}
		return nil
	case <-ctx.Done():
		signalTree(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("%w: %w", ErrSpeakFailed, ctx.Err())
	}
}
