package host

import (
	"context"
	"fmt"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/httpjson"
	"github.com/stevegeek/lever/internal/state"
	"github.com/stevegeek/lever/internal/wire"
)

func newBrokerCmd() *cobra.Command {
	c := &cobra.Command{Use: "broker", Short: "Run / control the capability broker"}
	c.AddCommand(newBrokerServeCmd(), newBrokerBumpEpochCmd())
	return c
}

func loadAppArg(args []string) (*config.App, error) {
	path, err := resolveConfigPath(argOrEmpty(args))
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// stateFor returns the broker state dir for an app (beside the config file).
func stateFor(path string) state.State {
	return state.ForConfig(filepath.Dir(path))
}

func newBrokerServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Run the capability broker + first-party tools (foreground)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argOrEmpty(args))
			if err != nil {
				return err
			}
			app, err := config.Load(path)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			cmd.Printf("broker %q serving on 127.0.0.1:%d (admin :%d)\n", app.Name, app.EffectiveJailPort(), app.EffectiveAdminPort())
			return brokerctl.Serve(ctx, app, stateFor(path), cli.VersionString(), brokerctl.ServeEnvFromOS())
		},
	}
}

func newBrokerBumpEpochCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bump-epoch [CONFIG]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Revoke all tokens at the current epoch (raise the floor)",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := loadAppArg(args)
			if err != nil {
				return err
			}
			return adminPost(cmd.Context(), app, wire.PathBumpEpoch, nil)
		},
	}
}

func newRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <agent> [CONFIG]",
		Args:  cobra.RangeArgs(1, 2),
		Short: "Revoke one agent on the running broker",
		RunE: func(cmd *cobra.Command, args []string) error {
			agent := args[0]
			app, err := loadAppArg(args[1:])
			if err != nil {
				return err
			}
			return adminPost(cmd.Context(), app, wire.PathRevoke, wire.RevokeRequest{Agent: agent})
		},
	}
}

// adminClient talks to the broker's loopback admin port. The port is local, so
// a short timeout bounds a wedged broker instead of hanging the CLI.
var adminClient = &http.Client{Timeout: 10 * time.Second}

// adminPost POSTs body as JSON to the running broker's loopback admin port
// (from config), discarding the response body.
func adminPost(ctx context.Context, app *config.App, path string, body any) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", app.EffectiveAdminPort(), path)
	err := httpjson.Post(ctx, adminClient, url, body, nil)
	if code := httpjson.Status(err); code != 0 {
		return fmt.Errorf("broker admin returned %d", code)
	}
	if err != nil {
		return fmt.Errorf("contacting broker admin (is `lever broker serve` running?): %w", err)
	}
	return nil
}
