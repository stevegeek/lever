package host

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

// vscodeSSHSettings are the VS Code settings a `lever ssh` host needs. The
// jail's egress blocks the agent from downloading VS Code's server, so the
// client downloads it and copies it over; the rest keep VS Code from opening
// paths between the agent's end and the operator's machine on its own (port
// forwards, the operator's ssh agent, X11).
var vscodeSSHSettings = [][2]string{
	{"remote.SSH.localServerDownload", `"always"`},
	{"remote.autoForwardPorts", "false"},
	{"remote.forwardOnOpen", "false"},
	{"remote.restoreForwardedPorts", "false"},
	{"remote.SSH.enableAgentForwarding", "false"},
	{"remote.SSH.enableX11Forwarding", "false"},
}

func newSSHConfigCmd() *cobra.Command {
	var configPath string
	return &cobra.Command{
		Use:   "ssh-config [--config PATH] [NAME...]",
		Short: "Print ssh Host blocks for `lever ssh`, for VS Code Remote-SSH (opt-in: ssh.enabled)",
		Long: `Print one ssh Host block per agent (the manager and every worker, or the
NAMEs given) whose ProxyCommand is ` + "`lever ssh --proxy`" + `, followed by the VS Code
settings Remote-SSH needs, as comments. Nothing is written: add the blocks to
~/.ssh/config yourself.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveConfigPath(configPath)
			if err != nil {
				return err
			}
			if p, err = filepath.Abs(p); err != nil {
				return err
			}
			app, err := config.Load(p)
			if err != nil {
				return err
			}
			st := stateFor(p)
			if err := sshAllowed(app, st); err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				names = []string{app.Name}
				for _, w := range app.Workers {
					names = append(names, w.Name)
				}
			}
			exe, err := leverExecutable()
			if err != nil {
				return fmt.Errorf("ssh-config: locating lever: %w", err)
			}
			return writeSSHConfig(cmd.OutOrStdout(), app, st, exe, p, names)
		},
	}
}

func writeSSHConfig(w io.Writer, app *config.App, st state.State, exe, configPath string, names []string) error {
	kh, err := sshConfigPath(st.SSHKnownHosts())
	if err != nil {
		return err
	}
	var b strings.Builder
	// ssh takes the first value it reads for most options, so a Host * block
	// above these (ForwardAgent yes, say) would win over them; and it adds up
	// RemoteForward, LocalForward, SendEnv and IdentityFile from every block,
	// so a Host * block below must exclude these hosts.
	b.WriteString("# lever ssh: put these blocks above any `Host *` block in ~/.ssh/config, and exclude\n" +
		"# lever.* from every Host * block (Host * !lever.*): ssh adds RemoteForward, LocalForward,\n" +
		"# SendEnv and IdentityFile from every block that matches, and a forward hands the agent a\n" +
		"# way into your machine.\n\n")
	for _, name := range names {
		slug, err := sshAgentName(app, name)
		if err != nil {
			return err
		}
		pc, err := sshProxyCommand(exe, configPath, slug)
		if err != nil {
			return err
		}
		alias := sshHostAlias(app, slug)
		fmt.Fprintf(&b, "Host %s\n  HostName %s\n  User %s\n", alias, alias, sshRemoteUser)
		for _, o := range sshClientOptions(alias, kh, pc) {
			fmt.Fprintf(&b, "  %s %s\n", o[0], o[1])
		}
		b.WriteString("\n")
	}
	b.WriteString("# VS Code settings.json for these hosts:\n")
	for _, s := range vscodeSSHSettings {
		fmt.Fprintf(&b, "#   %q: %s,\n", s[0], s[1])
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// sshConfigPath quotes a path for an ssh_config value, which splits on
// whitespace unless double-quoted and has no escape for a double quote.
func sshConfigPath(p string) (string, error) {
	if strings.ContainsAny(p, "\"\r\n%") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("ssh-config: %q cannot be written into an ssh config", p)
	}
	return `"` + p + `"`, nil
}
