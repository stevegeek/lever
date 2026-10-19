package host

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/brokerctl"
	"github.com/stevegeek/lever/internal/cli"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/logging"
	"github.com/stevegeek/lever/internal/servicehost"
	"gopkg.in/yaml.v3"
)

// The service host side ships in the host binary: the remote machine needs
// nothing lever does not already release, and no second program to keep in
// step with the broker's half of the pairing.
func newServiceHostCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "service-host",
		Short: "Run MCP servers and their credentials on another tailnet machine (see the service hosts guide)",
	}
	c.AddCommand(newServiceHostInitCmd(), newServiceHostServeCmd(), newServiceHostPairCmd())
	return c
}

func newServiceHostInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init SERVICE_HOST_CONFIG",
		Args:  cobra.ExactArgs(1),
		Short: "On the service host: make its server key (once) and print the fingerprint for lever.yaml",
		Long: "Makes the service host's server key and self-signed certificate, at the config's cert and key\n" +
			"(default server.crt and server.key beside it), unless both exist, and prints the key's fingerprint:\n" +
			"put it in the instance's broker.service_hosts[].fingerprint. The config need not be complete yet.",
		RunE: func(cmd *cobra.Command, args []string) error {
			certPath, keyPath, err := serviceHostKeyPaths(args[0])
			if err != nil {
				return err
			}
			fp, created, err := servicehost.EnsureKeyPair(certPath, keyPath, "lever-service-host", x509.ExtKeyUsageServerAuth, false)
			if err != nil {
				return err
			}
			if created {
				cmd.Printf("wrote %s (0600) and %s\n", keyPath, certPath)
			}
			cmd.Printf("server fingerprint: %s\n", fp)
			cmd.Println("put it in lever.yaml on the lever host: broker.service_hosts[].fingerprint")
			return nil
		},
	}
}

// serviceHostKeyPaths reads only cert and key from a config that may not be
// complete yet, so init can run before pairing.
func serviceHostKeyPaths(cfgPath string) (certPath, keyPath string, err error) {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Dir(abs)
	var partial struct {
		Cert string `yaml:"cert"`
		Key  string `yaml:"key"`
	}
	b, err := os.ReadFile(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", "", err
	default:
		if err := yaml.Unmarshal(b, &partial); err != nil {
			return "", "", fmt.Errorf("service host config %s: %w", cfgPath, err)
		}
	}
	at := func(p, def string) string {
		if p == "" {
			p = def
		}
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	return at(partial.Cert, "server.crt"), at(partial.Key, "server.key"), nil
}

func newServiceHostServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve SERVICE_HOST_CONFIG",
		Args:  cobra.ExactArgs(1),
		Short: "On the service host: serve its tools to the paired broker over mutual TLS (foreground)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := servicehost.LoadConfig(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return servicehost.Serve(ctx, cfg, logging.New(cmd.ErrOrStderr(), "service-host"), cli.VersionString())
		},
	}
}

func newServiceHostPairCmd() *cobra.Command {
	var rotate bool
	c := &cobra.Command{
		Use:   "pair NAME [CONFIG]",
		Args:  cobra.RangeArgs(1, 2),
		Short: "On the lever host: make the broker's client key for service host NAME and print its fingerprint",
		Long: "Makes the broker's client key and self-signed certificate for the declared service host NAME, in the\n" +
			"state directory (service-hosts/NAME/), unless they exist, and prints the key's fingerprint: put it in the\n" +
			"service host's client_fingerprint. --rotate replaces an existing key; restart the broker (lever apply) after.",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath(argAt(args, 1))
			if err != nil {
				return err
			}
			app, err := config.LoadNoHostChecks(path)
			if err != nil {
				return err
			}
			return runServiceHostPair(cmd, app, path, args[0], rotate)
		},
	}
	c.Flags().BoolVar(&rotate, "rotate", false, "replace an existing client key")
	return c
}

func argAt(args []string, i int) string {
	if len(args) > i {
		return args[i]
	}
	return ""
}

func runServiceHostPair(cmd *cobra.Command, app *config.App, path, name string, rotate bool) error {
	if _, ok := app.ServiceHost(name); !ok {
		return fmt.Errorf("service host %q is not declared in broker.service_hosts", name)
	}
	st := stateFor(path)
	if inside, err := brokerctl.StateInsideTreeErr(app, st); inside {
		if err == nil {
			err = errors.New("the state directory is inside the tree, where agents could read the key")
		}
		return err
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.ServiceHostDir(name)), 0o700); err != nil {
		return err
	}
	certPath, keyPath := st.ServiceHostClientCert(name), st.ServiceHostClientKey(name)
	fp, created, err := servicehost.EnsureKeyPair(certPath, keyPath, "lever-broker-"+app.Name, x509.ExtKeyUsageClientAuth, rotate)
	if err != nil {
		return err
	}
	if created {
		cmd.Printf("wrote %s (0600) and %s\n", keyPath, certPath)
	} else {
		cmd.Println("already paired (--rotate makes a new key)")
	}
	cmd.Printf("client fingerprint: %s\n", fp)
	cmd.Println("put it in the service host's config as client_fingerprint, then (re)start lever service-host serve there")
	if created {
		cmd.Println("then run lever apply here, so the broker loads the key")
	}
	return nil
}
