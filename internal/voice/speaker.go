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

// ErrSpeakNotSent: the text never reached the speech engine. No work was
// done, so its use of the limits is given back.
var ErrSpeakNotSent = errors.New("the text did not reach the speech engine")

// ErrSpeakFailed: the engine ran and gave no usable audio.
var ErrSpeakFailed = errors.New("the speech engine failed")

// Speaker is the speech engine as lever-tool-tts runs it: one run of
// sherpa-onnx-offline-tts per text as the tool's child, with the program and
// model checked before every run. No engine listens on any port.
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
	Log     func(format string, a ...any)

	once sync.Once
	run  chan struct{} // one run at a time

	mu      sync.Mutex
	real    string // the program's real path, pinned at its first good check
	stamp   string // speechStamp of the files at their last full verify that passed
	bad     string // speechStamp of the files at their last full verify that failed
	badErr  error
	checked time.Time
	// probed is the program and files the probe ran with, probeErr its
	// result; lexStamp and lex the files the lexicons were read from.
	probed   string
	probeErr error
	lexStamp string
	lex      map[string]Lexicon // by voice language
}

func (s *Speaker) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// Check runs before every run: the program must pass CheckServer and still
// resolve to the file it resolved to the first time, the model must verify,
// and the program must have spoken a short probe text with it (once for a
// program and its files: a sherpa-onnx older than 2.0 fails here). Files
// are hashed only when their stamp changed; a stamp that failed before
// fails again without a hash. It returns the program's real path.
func (s *Speaker) Check() (string, error) {
	real, err := s.checkFiles()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if key := real + "\x00" + s.stamp; s.probed != key {
		s.probeErr = s.probe(real)
		s.probed = key
		if s.probeErr != nil {
			s.logf("voice: %v", s.probeErr)
		}
	}
	if s.probeErr != nil {
		return "", s.probeErr
	}
	return real, nil
}

// probeText is what the probe speaks.
const probeText = "Ready."

// probe speaks probeText with the default voice and checks the WAV.
func (s *Speaker) probe(real string) error {
	v, ok := s.Model.Voice(s.Model.DefaultVoice)
	if !ok {
		return fmt.Errorf("the model %s has no voice %q", s.Model.Name, s.Model.DefaultVoice)
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.maxWait())
	defer cancel()
	if _, err := s.synth(ctx, real, v, probeText); err != nil {
		return fmt.Errorf("%s could not speak with the model %s (%v): lever needs sherpa-onnx 2.0 or later, which reads the model's lexicons (1.x refuses them)", real, s.Model.Name, err)
	}
	return nil
}

func (s *Speaker) maxWait() time.Duration {
	if s.MaxWait > 0 {
		return s.MaxWait
	}
	return 2 * time.Minute
}

// checkFiles is Check without the probe.
func (s *Speaker) checkFiles() (string, error) {
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
	_, err = VerifySpeech(s.ModelDir, s.Model)
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

func (s *Speaker) Ready() bool {
	_, err := s.Check()
	return err == nil
}

// Speak turns text into a WAV with voice ("" = the default), after
// SpeechInput. One run at a time; a caller waits until ctx ends. The error
// never carries the text.
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
	lex, err := s.lexicon(v)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the lexicons: %v", ErrSpeakNotSent, err)
	}
	if in = SpeakableText(in, lex); in == "" {
		return nil, fmt.Errorf("%w: %w: nothing to speak", ErrSpeakNotSent, ErrBadText)
	}
	ctx, cancel := context.WithTimeout(ctx, s.maxWait())
	defer cancel()
	return s.synth(ctx, real, v, in)
}

// lexicon is the words of v's lexicons, read once for the files that last
// verified.
func (s *Speaker) lexicon(v SpeechVoice) (Lexicon, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lex == nil || s.lexStamp != s.stamp {
		s.lex, s.lexStamp = map[string]Lexicon{}, s.stamp
	}
	if l, ok := s.lex[v.Lang]; ok {
		return l, nil
	}
	var paths []string
	for _, p := range s.Model.LexiconFiles(v) {
		paths = append(paths, SpeechPath(s.ModelDir, s.Model, p))
	}
	l, err := LoadLexicon(paths...)
	if err != nil {
		return nil, err
	}
	s.lex[v.Lang] = l
	return l, nil
}

// synth runs the engine once on text (which passed SpeechInput) and returns
// the checked WAV.
func (s *Speaker) synth(ctx context.Context, real string, v SpeechVoice, in string) ([]byte, error) {
	tmp, err := os.MkdirTemp(s.ModelDir, speakTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("%w: a private directory for the output: %v", ErrSpeakNotSent, err)
	}
	// The audio is on disk only while the engine writes it, in a 0700
	// directory of the tool's.
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

const speakTempPrefix = ".speak-"

// RemoveStaleSpeech deletes per-run output directories a killed tool left in
// dir, once untouched for longer than any run may take (another tool sharing
// the directory may be running one). Best effort.
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

const staleSpeechAfter = 10 * time.Minute

// runEngine runs the program once: no stdin, stdout and stderr discarded
// (the program prints the text, assumption 5), a reduced environment, its
// own process group, and Pdeathsig on Linux. When ctx ends first the group
// is killed.
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
