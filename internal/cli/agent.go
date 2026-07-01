package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lever-to/lever/internal/agent"
	"github.com/lever-to/lever/internal/config"
	"github.com/lever-to/lever/internal/scion"
	"github.com/spf13/cobra"
)

// loadManifest reads the sanitized runtime manifest; a package var so tests can
// inject a fake without touching the filesystem.
var loadManifest = config.LoadManifest

// ProvisionFunc provisions a grove with the broker and returns a Bootstrap
// (ticket + broker CA + broker URL + agent CN) ready to be staged for the grove.
// Tests inject a fake; production uses realProvision.
type ProvisionFunc func(ctx context.Context, grove string) (agent.Bootstrap, error)

// provisionGrove is the active provisioner. The production default is
// realProvision, which degrades gracefully (returns empty Bootstrap, nil error)
// when the manager has no broker configured (bootstrap file absent). Tests
// inject a fake via the package-level seam. A nil value is also safe: agentStart
// skips staging entirely, preserving existing non-brokered behaviour.
var provisionGrove ProvisionFunc = realProvision

// managerBootstrapPath is where the manager's own bootstrap.json is readable
// from inside the manager CONTAINER, where realProvision runs. scion mounts the
// manager's tree at /workspace in the container (the jail-level /lever mount does
// not exist there), so the bootstrap deposited by `lever apply` at
// <tree>/.lever/bootstrap.json appears here. Tests can override it.
var managerBootstrapPath = "/workspace/.lever/bootstrap.json"

// managerIDDir is the directory holding the manager's mTLS identity (cert+key+ca).
// It is the "~/.lever-id" path resolved at process start. Tests can override it.
var managerIDDir = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lever-id")
}()

// realProvision builds an mTLS client from the manager's identity and the broker
// CA/URL read from the manager's own bootstrap, then POSTs /provision to mint a
// one-use enrolment ticket for the given grove. It assembles the full Bootstrap
// (BrokerCA + BrokerURL come from the manager's bootstrap so the grove agent can
// trust the same CA and reach the same broker).
//
// If the manager's bootstrap file does not exist (no broker configured), it
// returns an empty Bootstrap with a nil error so agentStart skips staging and
// non-brokered groves continue to work unchanged.
func realProvision(ctx context.Context, grove string) (agent.Bootstrap, error) {
	bs, err := agent.LoadBootstrap(managerBootstrapPath)
	if err != nil {
		// LoadBootstrap wraps the os error with %w, so errors.Is sees through it.
		if errors.Is(err, os.ErrNotExist) {
			return agent.Bootstrap{}, nil // no broker configured — skip provisioning
		}
		return agent.Bootstrap{}, fmt.Errorf("manager bootstrap: %w", err)
	}
	id, ok := agent.LoadIdentity(managerIDDir)
	if !ok {
		return agent.Bootstrap{}, fmt.Errorf("manager identity not found in %s", managerIDDir)
	}
	httpClient, err := id.Client()
	if err != nil {
		return agent.Bootstrap{}, fmt.Errorf("manager mTLS client: %w", err)
	}
	body, err := json.Marshal(map[string]string{"grove": grove})
	if err != nil {
		return agent.Bootstrap{}, fmt.Errorf("provision marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", bs.BrokerURL+"/provision", bytes.NewReader(body))
	if err != nil {
		return agent.Bootstrap{}, fmt.Errorf("provision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return agent.Bootstrap{}, fmt.Errorf("provision: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return agent.Bootstrap{}, fmt.Errorf("provision: broker returned %d", resp.StatusCode)
	}
	var pr struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return agent.Bootstrap{}, fmt.Errorf("provision decode: %w", err)
	}
	return agent.Bootstrap{
		Ticket:    pr.Ticket,
		BrokerCA:  bs.BrokerCA,
		BrokerURL: bs.BrokerURL,
		AgentCN:   grove,
	}, nil
}

// stageGroveBootstrap writes a Bootstrap to <groveProject>/.lever/bootstrap.json
// (dir 0700, file 0600). This is the file that lever-agent boot reads inside the
// grove container (via its canonical default path: ./.lever/bootstrap.json relative
// to the container's workspace CWD — no LEVER_BOOTSTRAP env injection needed).
func stageGroveBootstrap(groveProject string, bs agent.Bootstrap) error {
	dir := filepath.Join(groveProject, ".lever")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("stage bootstrap: mkdir: %w", err)
	}
	b, err := json.Marshal(bs)
	if err != nil {
		return fmt.Errorf("stage bootstrap: marshal: %w", err)
	}
	p := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return fmt.Errorf("stage bootstrap: write: %w", err)
	}
	// Chmod tightens a pre-existing file: WriteFile truncates but keeps the
	// existing mode on a file that already exists, so a re-stage would leave a
	// world-readable bootstrap.json if it was originally created with loose perms.
	if err := os.Chmod(p, 0o600); err != nil {
		return fmt.Errorf("stage bootstrap: chmod: %w", err)
	}
	return nil
}

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

// resolveGroveLLMAuth looks up a grove's effective LLM-auth mode from the
// sanitized runtime manifest (the only config the in-jail manager has). Empty
// path, an unreadable manifest path that doesn't exist, or a grove absent from
// the manifest ⇒ "" (caller treats as not-api-key — the safe default). A
// present-but-unparseable manifest is a real error.
func resolveGroveLLMAuth(path, grove string) (config.LLMAuthMode, error) {
	if path == "" {
		return "", nil
	}
	m, err := loadManifest(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return m.LLMAuthFor(grove), nil
}

// manifestMountRoot reads the jail mount root from the runtime manifest (empty
// path, an unreadable/absent file, or no field ⇒ ""). The in-jail manager joins
// it with a grove's tree-relative dir to form the jail-absolute path scion needs.
func manifestMountRoot(path string) string {
	if path == "" {
		return ""
	}
	m, err := loadManifest(path)
	if err != nil {
		return ""
	}
	return m.MountRoot
}

// scionGroveProject translates a grove's tree-relative dir into its jail-absolute
// path (mountRoot + dir), which scion needs for both `-g` and `--workspace`. An
// empty mount root or an already-absolute dir is returned unchanged. Without this
// scion falls back to mounting the manager's whole tree at the grove's
// /workspace, so the grove reads the manager's bootstrap and cannot enrol.
func scionGroveProject(mountRoot, dir string) string {
	if mountRoot == "" || dir == "" || filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(mountRoot, dir)
}

// groveAgentPhase returns the scion phase of the named grove agent in project
// (""=absent). Mirrors managerPhase; List tolerates empty output, so a hub with
// no such agent yields absent rather than an error.
func groveAgentPhase(ctx context.Context, c *scion.Client, project, grove string) (string, error) {
	agents, err := c.List(ctx, project)
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Slug == grove {
			return a.Phase, nil
		}
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
			grove := args[0]

			// Resolve the manifest path once — it feeds both image and LLM-auth
			// lookup. With no --manifest set, fall back to the default location.
			if manifestPath == "" {
				manifestPath = defaultManifestPath()
			}

			// An explicit --image wins. Otherwise resolve from the sanitized
			// runtime manifest the host wrote into the mount (grove→image), so the
			// caller doesn't repeat the image on every dispatch.
			if image == "" {
				resolved, err := resolveGroveImage(manifestPath, grove)
				if err != nil {
					return err
				}
				image = resolved
			}

			// Translate the grove's tree-relative dir into its jail-absolute path
			// for scion (-g + --workspace + the phase probe below). The bootstrap is
			// staged via the relative `project` (the manager container's filesystem
			// view), but scion runs in the jail and needs the jail path; without
			// --workspace it would mount the manager's whole tree at the grove's
			// /workspace and the grove would read the manager's bootstrap.
			scionProject := scionGroveProject(manifestMountRoot(manifestPath), project)

			// Idempotent: if the grove agent already exists, resume it rather than
			// 409 on a fresh `start`. A stopped/suspended agent already holds its
			// enrolled identity, so it is NOT re-provisioned; only an absent agent is
			// provisioned + started below. (`scion delete` remains the hard reset.)
			phase, err := groveAgentPhase(cmd.Context(), cf(), scionProject, grove)
			if err != nil {
				return err
			}
			switch phase {
			case "running":
				cmd.Printf("%s is already running.\n", grove)
				return nil
			case "suspended", "stopped":
				if err := cf().Resume(cmd.Context(), grove, scionProject); err != nil {
					return err
				}
				cmd.Printf("Resumed %s.\n", grove)
				return nil
			}

			// Absent: provision (mint a one-use enrolment ticket) and stage the
			// bootstrap in the grove's workspace BEFORE starting the container. The
			// grove's lever-agent boot reads it from /workspace/.lever/bootstrap.json
			// (the hook passes that absolute path). provisionGrove is nil when no
			// broker is configured, in which case staging is skipped so non-brokered
			// groves continue to work unchanged.
			if provisionGrove != nil && project != "" {
				bs, err := provisionGrove(cmd.Context(), grove)
				if err != nil {
					return fmt.Errorf("provision grove %s: %w", grove, err)
				}
				if bs.Ticket != "" { // empty ⇒ no broker configured ⇒ skip staging
					if err := stageGroveBootstrap(project, bs); err != nil {
						return err
					}
				}
			}

			// api-key mode: the grove's effective mode comes from the sanitized
			// manifest — the only config available in the jail.
			mode, err := resolveGroveLLMAuth(manifestPath, grove)
			if err != nil {
				return err
			}
			apiKey := mode == config.LLMAuthAPIKey
			// Convey LEVER_LLM_AUTH=api-key to the grove container so its pre-start
			// hook enters api-key mode (the hook reads $LEVER_LLM_AUTH; scion projects
			// Hub env before pre-start hooks run). Project-scoped to this grove's dir
			// so it never leaks to other agents. Set BEFORE start. Needs a project dir
			// to scope to; a project-less ad-hoc start can't be isolated, so it stays
			// subscription (the safe default).
			if apiKey && scionProject != "" {
				if err := cf().EnvSet(cmd.Context(), scionProject, "LEVER_LLM_AUTH", "api-key"); err != nil {
					return fmt.Errorf("set LEVER_LLM_AUTH for grove %s: %w", grove, err)
				}
			}

			// api-key: start with --harness-auth api-key (satisfied by the placeholder
			// ANTHROPIC_API_KEY Hub secret set at apply time); the real credential
			// arrives in-container via the broker /llm capability.
			if err := cf().Start(cmd.Context(), scion.StartOpts{Grove: grove, Task: task, Harness: "claude", Project: scionProject, Workspace: scionProject, Image: image, APIKey: apiKey}); err != nil {
				return err
			}
			cmd.Printf("Started %s.\n", grove)
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
