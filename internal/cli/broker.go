package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/lever-to/lever/internal/brokerctl"
	"github.com/lever-to/lever/internal/config"
	"github.com/spf13/cobra"
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
func stateFor(path string) brokerctl.State {
	return brokerctl.StateDir(filepath.Dir(path))
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
			return brokerctl.Serve(ctx, app, stateFor(path))
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
			return adminPost(cmd.Context(), app, "/bump-epoch", nil)
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
			return adminPost(cmd.Context(), app, "/revoke", []byte(fmt.Sprintf(`{"agent":%q}`, agent)))
		},
	}
}

// adminPost POSTs to the running broker's loopback admin port (from config).
func adminPost(ctx context.Context, app *config.App, path string, body []byte) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", app.EffectiveAdminPort(), path)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting broker admin (is `lever broker serve` running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("broker admin returned %d", resp.StatusCode)
	}
	return nil
}
