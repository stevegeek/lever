package cli

import (
	"os"
	"path/filepath"

	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

// loadManifest reads the sanitized runtime manifest; a package var so tests can
// inject a fake without touching the filesystem.
var loadManifest = config.LoadManifest

// resolveGroveImage looks up a grove's image from the sanitized runtime manifest
// the host wrote into the mount (grove→image only — no host config in the jail).
// Empty path ⇒ "" (let scion decide / require --image). A grove absent from the
// manifest also ⇒ "" — an ad-hoc grove not declared in the config must pass an
// explicit --image. A present-but-unreadable manifest is a real error.
func resolveGroveImage(path, grove string) (string, error) {
	if path == "" {
		return "", nil
	}
	m, err := loadManifest(path)
	if err != nil {
		return "", err
	}
	if img, ok := m.ImageFor(grove); ok {
		return img, nil
	}
	return "", nil
}

// defaultManifestPath is the manifest location the manager reads: $LEVER_MANIFEST
// if set, else ManifestName in the current directory (the in-jail mount root).
func defaultManifestPath() string {
	if p := os.Getenv("LEVER_MANIFEST"); p != "" {
		return p
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	p := filepath.Join(wd, config.ManifestName)
	if fi, statErr := os.Stat(p); statErr != nil || fi.IsDir() {
		return ""
	}
	return p
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
	var project, image, task, manifestPath string
	c := &cobra.Command{Use: "start NAME", Args: cobra.ExactArgs(1), Short: "Start (or resume-on-suspended) a grove agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			// An explicit --image wins. Otherwise resolve from the sanitized
			// runtime manifest the host wrote into the mount (grove→image), so the
			// caller doesn't repeat the image on every dispatch. With no --manifest
			// set, fall back to the default manifest location.
			if image == "" {
				if manifestPath == "" {
					manifestPath = defaultManifestPath()
				}
				resolved, err := resolveGroveImage(manifestPath, args[0])
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
	c.Flags().StringVar(&image, "image", "", "agent image (overrides the manifest)")
	c.Flags().StringVar(&manifestPath, "manifest", "", "runtime manifest for grove image lookup (default $LEVER_MANIFEST or ./"+config.ManifestName+")")
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
