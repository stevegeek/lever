package host

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/voice"
)

func newVoiceCmd() *cobra.Command {
	c := &cobra.Command{Use: "voice", Short: "Dictation on the chat page (remote.voice): the Whisper model"}
	c.AddCommand(newVoiceFetchCmd())
	return c
}

func newVoiceFetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch [MODEL] [CONFIG]",
		Args:  cobra.MaximumNArgs(2),
		Short: "Download a pinned Whisper model into the state directory, checking its size and sha256",
		Long: "Downloads MODEL (default: remote.voice.model, or " + voice.DefaultModel + ") from Hugging Face at the commit lever pins,\n" +
			"into the state directory, and keeps it only if its size and sha256 match lever's table. Models: " +
			strings.Join(voice.Names(), ", ") + ".\nThe remote proxy never downloads a model by itself; restart it after a fetch.",
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
			return runVoiceFetch(cmd.Context(), app, stateFor(path), name, voice.Fetcher{Progress: cmd.OutOrStdout()}.Fetch, cmd.OutOrStdout())
		},
	}
}

// runVoiceFetch fetches model name ("" = the configured one) with fetch
// into the state directory, which must be outside the tree.
func runVoiceFetch(ctx context.Context, app *config.App, st state.State, name string,
	fetch func(context.Context, voice.Model, string) (string, error), out io.Writer) error {
	if name == "" {
		name = app.EffectiveVoiceModel()
	}
	m, ok := voice.Lookup(name)
	if !ok {
		return fmt.Errorf("voice: %q is not in lever's pinned model table; use one of %s", name, strings.Join(voice.Names(), ", "))
	}
	if why := m.Pinned(); why != "" {
		return fmt.Errorf("voice: %s; this lever release cannot download it", why)
	}
	if brokerctl.StateInsideTree(app, st) {
		return fmt.Errorf("voice: the state directory %s is inside the tree, where agents could replace the model; "+
			"point `tree:` at a subdirectory that does not contain it", st.Dir)
	}
	fmt.Fprintf(out, "fetching %s (%d bytes) from %s\n", m.Name, m.Size, m.URL())
	p, err := fetch(ctx, m, st.VoiceModels())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s verified: %s (sha256 %s)\n", m.Name, p, m.SHA256)
	if !app.VoiceOn() || app.EffectiveVoiceModel() != m.Name {
		fmt.Fprintf(out, "set remote.voice.model: %s (and enabled: true) to use it\n", m.Name)
	} else {
		fmt.Fprintln(out, "restart the remote proxy to use it (lever stop, then lever up)")
	}
	return nil
}
