package host

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/voice"
)

func newVoiceCmd() *cobra.Command {
	c := &cobra.Command{Use: "voice", Short: "Speech on the host (lever-tool-whisper, lever-tool-tts, remote.voice): the pinned models"}
	c.AddCommand(newVoiceFetchCmd())
	return c
}

func newVoiceFetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch [MODEL] [CONFIG]",
		Args:  cobra.MaximumNArgs(2),
		Short: "Download a pinned Whisper or speech model for lever-tool-whisper or lever-tool-tts, checking its size and sha256",
		Long: "Downloads MODEL and keeps it only if its size and sha256 match lever's table.\n" +
			"Whisper models (lever-tool-whisper), one file each from Hugging Face at the commit lever pins: " + strings.Join(voice.Names(), ", ") + ".\n" +
			"Speech models (lever-tool-tts), one archive each from sherpa-onnx's GitHub releases, unpacked once verified,\n" +
			"with a manifest of every file's size and sha256: " + strings.Join(voice.SpeechNames(), ", ") + ".\n" +
			"Without MODEL: the -model of the whisper tool (or " + voice.DefaultModel + "), and the -model of the tts tool when one is configured.\n" +
			"A model goes into the -models directory of its tool's entry in broker.tools, or, with none configured,\n" +
			"into voice-models/ in the state directory (point the tool's -models at that absolute path).\n" +
			"The tools never download a model by themselves; they pick up a fetched model without a restart.",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, cfgArg := "", ""
			switch {
			case len(args) == 2:
				name, cfgArg = args[0], args[1]
			case len(args) == 1:
				_, whisper := voice.Lookup(args[0])
				_, speech := voice.LookupSpeech(args[0])
				if whisper || speech {
					name = args[0]
				} else {
					cfgArg = args[0]
				}
			}
			path, err := resolveConfigPath(cfgArg)
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			// Ctrl-C ends the download and removes the temp file.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			f := voice.Fetcher{Progress: cmd.OutOrStdout()}
			st, out := stateFor(path), cmd.OutOrStdout()
			if _, speech := voice.LookupSpeech(name); speech {
				return runSpeechFetch(ctx, app, st, name, f.FetchSpeech, out)
			}
			if name != "" || len(app.WhisperTools()) > 0 || len(app.TTSTools()) == 0 {
				if err := runVoiceFetch(ctx, app, st, name, f.Fetch, out); err != nil {
					return err
				}
			}
			if name == "" && len(app.TTSTools()) > 0 {
				return runSpeechFetch(ctx, app, st, "", f.FetchSpeech, out)
			}
			return nil
		},
	}
}

// voiceFetchTarget is dictation's whisper tool, else the first one, else
// voice-models/ in the state directory (ok false: no tool).
func voiceFetchTarget(app *config.App, st state.State) (dir string, w config.WhisperTool, ok bool, err error) {
	w, ok = app.VoiceTool()
	if !ok {
		if ws := app.WhisperTools(); len(ws) > 0 {
			w, ok = ws[0], true
		}
	}
	if ok && w.Models != "" {
		// Config load refused a -models inside the tree.
		if !filepath.IsAbs(w.Models) {
			return "", w, ok, fmt.Errorf("voice: broker tool %q's -models %q must be an absolute path", w.Name, w.Models)
		}
		return w.Models, w, ok, nil
	}
	if brokerctl.StateInsideTree(app, st) {
		return "", w, ok, fmt.Errorf("voice: the state directory %s is inside the tree, where agents could replace the model; "+
			"point `tree:` at a subdirectory that does not contain it", st.Dir)
	}
	return st.VoiceModels(), w, ok, nil
}

func runVoiceFetch(ctx context.Context, app *config.App, st state.State, name string,
	fetch func(context.Context, voice.Model, string) (string, error), out io.Writer) error {
	dir, w, ok, err := voiceFetchTarget(app, st)
	if err != nil {
		return err
	}
	if name == "" {
		name = voice.DefaultModel
		if ok {
			name = w.EffectiveModel()
		}
	}
	m, found := voice.Lookup(name)
	if !found {
		return fmt.Errorf("voice: %q is not in lever's pinned model table; use one of %s", name, strings.Join(voice.Names(), ", "))
	}
	if why := m.Pinned(); why != "" {
		return fmt.Errorf("voice: %s; this lever release cannot download it", why)
	}
	fmt.Fprintf(out, "fetching %s (%d bytes) from %s\n", m.Name, m.Size, m.URL())
	p, err := fetch(ctx, m, dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s verified: %s (sha256 %s)\n", m.Name, p, m.SHA256)
	switch {
	case !ok:
		fmt.Fprintf(out, "add lever-tool-whisper to broker.tools with -models %s and -model %s to use it\n", dir, m.Name)
	case w.EffectiveModel() != m.Name:
		fmt.Fprintf(out, "set -model %s in broker tool %q's command to use it, then lever reload\n", m.Name, w.Name)
	default:
		fmt.Fprintf(out, "broker tool %q picks it up within a minute (lever doctor shows when it is ready)\n", w.Name)
	}
	return nil
}

// speechFetchTarget is read-aloud's tts tool, else the first one, else
// voice-models/ in the state directory, one directory per model (ok false: no
// tool).
func speechFetchTarget(app *config.App, st state.State) (dir string, t config.TTSTool, ok bool, err error) {
	t, ok = app.SpeechTool()
	if !ok {
		if ts := app.TTSTools(); len(ts) > 0 {
			t, ok = ts[0], true
		}
	}
	if ok && t.Models != "" {
		if !filepath.IsAbs(t.Models) {
			return "", t, ok, fmt.Errorf("voice: broker tool %q's -models %q must be an absolute path", t.Name, t.Models)
		}
		return t.Models, t, ok, nil
	}
	if brokerctl.StateInsideTree(app, st) {
		return "", t, ok, fmt.Errorf("voice: the state directory %s is inside the tree, where agents could replace the model; "+
			"point `tree:` at a subdirectory that does not contain it", st.Dir)
	}
	return st.VoiceModels(), t, ok, nil
}

func runSpeechFetch(ctx context.Context, app *config.App, st state.State, name string,
	fetch func(context.Context, voice.SpeechModel, string) (string, error), out io.Writer) error {
	dir, t, ok, err := speechFetchTarget(app, st)
	if err != nil {
		return err
	}
	if name == "" {
		name = voice.DefaultSpeechModel
		if ok {
			name = t.EffectiveModel()
		}
	}
	m, found := voice.LookupSpeech(name)
	if !found {
		return fmt.Errorf("voice: %q is not in lever's pinned speech model table; use one of %s", name, strings.Join(voice.SpeechNames(), ", "))
	}
	if why := m.Pinned(); why != "" {
		return fmt.Errorf("voice: %s; this lever release cannot download it", why)
	}
	fmt.Fprintf(out, "fetching %s (an archive of %d bytes) from %s\n", m.Name, m.ArchiveSize, m.ArchiveURL)
	p, err := fetch(ctx, m, dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s verified: %s (unpacked from the archive of sha256 %s)\n", m.Name, p, m.ArchiveSHA256)
	switch {
	case !ok:
		fmt.Fprintf(out, "add lever-tool-tts to broker.tools with -models %s and -model %s to use it\n", dir, m.Name)
	case t.EffectiveModel() != m.Name:
		fmt.Fprintf(out, "set -model %s in broker tool %q's command to use it, then lever reload\n", m.Name, t.Name)
	default:
		fmt.Fprintf(out, "broker tool %q picks it up at its next request (lever doctor shows when it is ready)\n", t.Name)
	}
	return nil
}
