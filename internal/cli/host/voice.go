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
	c := &cobra.Command{Use: "voice", Short: "Speech to text (lever-tool-whisper, remote.voice): the Whisper model"}
	c.AddCommand(newVoiceFetchCmd())
	return c
}

func newVoiceFetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch [MODEL] [CONFIG]",
		Args:  cobra.MaximumNArgs(2),
		Short: "Download a pinned Whisper model for lever-tool-whisper, checking its size and sha256",
		Long: "Downloads MODEL (default: the whisper tool's -model, or " + voice.DefaultModel + ") from Hugging Face at the commit lever pins,\n" +
			"and keeps it only if its size and sha256 match lever's table. Models: " + strings.Join(voice.Names(), ", ") + ".\n" +
			"It goes into the -models directory of the lever-tool-whisper entry in broker.tools, or, with none configured,\n" +
			"into voice-models/ in the state directory (point the tool's -models at that absolute path).\n" +
			"The tool never downloads a model by itself; it picks up a fetched model within a minute.",
		RunE: func(cmd *cobra.Command, args []string) error {
			name, cfgArg := "", ""
			switch {
			case len(args) == 2:
				name, cfgArg = args[0], args[1]
			case len(args) == 1:
				if _, ok := voice.Lookup(args[0]); ok {
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
			// Ctrl-C ends the download, and the temp file goes with it.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runVoiceFetch(ctx, app, stateFor(path), name, voice.Fetcher{Progress: cmd.OutOrStdout()}.Fetch, cmd.OutOrStdout())
		},
	}
}

// voiceFetchTarget is where a fetch goes, and the tool it is for (ok
// false: none configured): dictation's whisper tool, else the first one,
// else voice-models/ in the state directory.
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

// runVoiceFetch fetches model name ("" = the whisper tool's -model, or the
// default) with fetch into voiceFetchTarget.
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
