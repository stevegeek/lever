package cli

import (
	"os"

	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

// loadAppConfig is the config loader used to resolve a grove's image; a package
// var so tests can inject a fake without touching the filesystem.
var loadAppConfig = config.Load

// resolveGroveImage looks up the image a grove should run on from the lever
// config at path. Empty path ⇒ "" (no config; let scion default decide). A
// grove absent from the config also ⇒ "" (caller may pass --image explicitly).
// A set-but-unreadable path is a real misconfiguration and returns an error.
func resolveGroveImage(path, grove string) (string, error) {
	if path == "" {
		return "", nil
	}
	app, err := loadAppConfig(path)
	if err != nil {
		return "", err
	}
	g, ok := app.GroveByName(grove)
	if !ok {
		return "", nil
	}
	return app.GroveImage(g), nil
}

func newAgentCmd(cf ClientFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Drive grove agents on Scion"}
	cmd.AddCommand(
		agentList(cf), agentStart(cf), agentStop(cf), agentSuspend(cf),
		agentResume(cf), agentAttach(cf), agentRegister(cf),
	)
	return cmd
}

func projectFlagVar(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVarP(p, "project", "g", "", "grove project path (-g)")
}

func agentList(cf ClientFactory) *cobra.Command {
	var project string
	c := &cobra.Command{Use: "list", Short: "List a grove's agents", RunE: func(cmd *cobra.Command, _ []string) error {
		agents, err := cf().List(cmd.Context(), project)
		if err != nil {
			return err
		}
		if len(agents) == 0 {
			cmd.Println("No running agents.")
			return nil
		}
		for _, a := range agents {
			line := "  " + a.Slug + "  [" + a.Phase + "]"
			if a.Activity != "" {
				line += "  — " + a.Activity
			}
			cmd.Println(line)
		}
		return nil
	}}
	projectFlagVar(c, &project)
	return c
}

func agentStart(cf ClientFactory) *cobra.Command {
	var project, image, task, configPath string
	c := &cobra.Command{Use: "start NAME", Args: cobra.ExactArgs(1), Short: "Start (or resume-on-suspended) a grove agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			// An explicit --image wins. Otherwise resolve from the lever config
			// (the grove's `image:` or the manager image it inherits), so the
			// caller doesn't have to repeat the image on every dispatch.
			if image == "" {
				resolved, err := resolveGroveImage(configPath, args[0])
				if err != nil {
					return err
				}
				image = resolved
			}
			if err := cf().Start(cmd.Context(), scion.StartOpts{Grove: args[0], Task: task, Harness: "claude", Project: project, Image: image}); err != nil {
				return err
			}
			cmd.Printf("Started %s.\n", args[0])
			return nil
		}}
	projectFlagVar(c, &project)
	c.Flags().StringVar(&image, "image", "", "agent image (overrides config)")
	c.Flags().StringVar(&configPath, "config", os.Getenv("LEVER_CONFIG"), "lever config for grove image lookup (default $LEVER_CONFIG)")
	c.Flags().StringVar(&task, "task", "Read your context, then begin.", "task/boot prompt")
	return c
}

func simpleLifecycle(use, short string, fn func(*scion.Client, *cobra.Command, string, string) error, cf ClientFactory) *cobra.Command {
	var project string
	c := &cobra.Command{Use: use + " NAME", Args: cobra.ExactArgs(1), Short: short,
		RunE: func(cmd *cobra.Command, args []string) error { return fn(cf(), cmd, args[0], project) }}
	projectFlagVar(c, &project)
	return c
}

func agentStop(cf ClientFactory) *cobra.Command {
	return simpleLifecycle("stop", "Stop an agent (fresh next start)", func(c *scion.Client, cmd *cobra.Command, n, p string) error {
		if err := c.Stop(cmd.Context(), n, p); err != nil {
			return err
		}
		cmd.Printf("Stopped %s.\n", n)
		return nil
	}, cf)
}
func agentSuspend(cf ClientFactory) *cobra.Command {
	return simpleLifecycle("suspend", "Suspend an agent (keep conversation)", func(c *scion.Client, cmd *cobra.Command, n, p string) error {
		if err := c.Suspend(cmd.Context(), n, p); err != nil {
			return err
		}
		cmd.Printf("Suspended %s.\n", n)
		return nil
	}, cf)
}
func agentResume(cf ClientFactory) *cobra.Command {
	return simpleLifecycle("resume", "Resume a suspended agent", func(c *scion.Client, cmd *cobra.Command, n, p string) error {
		if err := c.Resume(cmd.Context(), n, p); err != nil {
			return err
		}
		cmd.Printf("Resumed %s.\n", n)
		return nil
	}, cf)
}

func agentAttach(cf ClientFactory) *cobra.Command {
	var project string
	c := &cobra.Command{Use: "attach NAME", Args: cobra.ExactArgs(1), Short: "Print the attach argv (caller execs it)",
		RunE: func(cmd *cobra.Command, args []string) error {
			argv := cf().AttachArgv(args[0], project)
			for i, a := range argv {
				if i > 0 {
					cmd.Print(" ")
				}
				cmd.Print(a)
			}
			cmd.Println()
			return nil
		}}
	projectFlagVar(c, &project)
	return c
}

func agentRegister(cf ClientFactory) *cobra.Command {
	c := &cobra.Command{Use: "register DIR", Args: cobra.ExactArgs(1), Short: "Onboard a non-git directory as a Scion project (init + hub link)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := cf()
			if err := client.InitProject(cmd.Context(), args[0]); err != nil {
				return err
			}
			if err := client.HubLink(cmd.Context(), args[0]); err != nil {
				return err
			}
			cmd.Printf("Registered %s as a Scion project.\n", args[0])
			return nil
		}}
	return c
}
