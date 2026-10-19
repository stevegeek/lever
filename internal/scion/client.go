// Package scion is the single seam to the scion CLI. It builds argv and env
// and delegates execution to an injected proc.Runner.
//
// It owns everything about running scion: subcommand names, flags, env vars,
// the hub's default port, and every predicate over scion's output wording. No
// other package may match scion's error text; add a predicate here instead.
// scion's on-disk shape lives in internal/scion/layout, and the hub's REST
// surface in internal/hubapi.
package scion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/wait"
)

// DefaultHubEndpoint is the hub endpoint on the loopback of whatever runs the
// request. Every caller must resolve it inside the jail: the hub binds the
// jail's loopback and the Lima backend suppresses guest-to-host port
// forwarding by design. It is host-reachable on OrbStack only, by accident.
//
// New applies no default: an empty Options.HubEndpoint omits
// SCION_HUB_ENDPOINT, deferring to the scion binary's own default.
const DefaultHubEndpoint = "http://127.0.0.1:8080"

// DefaultHubPort is scion's own --web-port default. A test pins it to
// DefaultHubEndpoint.
const DefaultHubPort = 8080

type Options struct {
	Bin            string        // default "scion"
	HubEndpoint    string        // SCION_HUB_ENDPOINT
	HubTokenSource func() string // SCION_HUB_TOKEN (lazy: the controller PAT is minted mid-apply)
	// AgentRole is passed as scion start --role. Empty omits the flag, which
	// a scion without roles does not accept.
	AgentRole string
	// SettingsWarnLog receives each distinct scion settings-keys warning the
	// client filters out of its output (IsSettingsKeysWarning). nil drops it.
	SettingsWarnLog func(line string)
}

type Client struct {
	bin            string
	hubEndpoint    string
	hubTokenSource func() string
	agentRole      string
	r              proc.Runner
	warnLog        *warnLog

	// Wait budgets; tests shrink them.
	hubReady      wait.Spec
	hubAliveEvery time.Duration
	hubRetryPause time.Duration
}

func New(r proc.Runner, o Options) *Client {
	bin := o.Bin
	if bin == "" {
		bin = "scion"
	}
	return &Client{
		bin:            bin,
		hubEndpoint:    o.HubEndpoint,
		hubTokenSource: o.HubTokenSource,
		agentRole:      o.AgentRole,
		r:              r,
		warnLog:        &warnLog{sink: o.SettingsWarnLog},

		hubReady:      wait.HubReady,
		hubAliveEvery: hubAliveEvery,
		hubRetryPause: wait.HubStartRetryPause,
	}
}

// currentHubToken reads the source at call time: the controller PAT is minted
// after New.
func (c *Client) currentHubToken() string {
	if c.hubTokenSource != nil {
		return c.hubTokenSource()
	}
	return ""
}

// env selects the hub by SCION_HUB_ENDPOINT alone: it puts scion in hub
// context on every pin lever supports, whatever the settings files say.
func (c *Client) env() map[string]string {
	m := map[string]string{}
	if c.hubEndpoint != "" {
		m["SCION_HUB_ENDPOINT"] = c.hubEndpoint
	}
	if tok := c.currentHubToken(); tok != "" {
		m["SCION_HUB_TOKEN"] = tok
	}
	return m
}

// nonInteractive is on every scion command lever runs without a terminal.
// scion answers a prompt with no terminal by its default, which is often No;
// with the flag, a prompt either takes its Yes path or fails naming the flag
// it needs, and never waits on stdin.
const nonInteractive = "--non-interactive"

func projectFlag(project string) []string {
	if project == "" {
		return nil
	}
	return []string{"-g", project}
}

// runValue is run for a command whose stdout is a value lever parses. It
// returns stdout alone: scion's settings loader writes warnings to stderr,
// which run would prepend to the value and silently change a comparison.
// Errors still carry both streams.
func (c *Client) runValue(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := c.r.RunIn(ctx, dir, c.env(), c.bin, args...)
	if err != nil {
		return "", fmt.Errorf("scion %s: %s", redactArgs(args), cleanErr(c.filterOutput(res.Stdout)+c.filterOutput(res.Stderr)))
	}
	c.filterOutput(res.Stderr) // logged, never part of the value
	return strings.TrimSpace(res.Stdout), nil
}

func (c *Client) run(ctx context.Context, dir string, args ...string) (string, error) {
	return c.runEnv(ctx, dir, nil, args...)
}

// runEnv is run with extra env on top of the client's; extra never sets the
// hub token, endpoint or switch, which only the client's own options decide.
func (c *Client) runEnv(ctx context.Context, dir string, extra map[string]string, args ...string) (string, error) {
	env := c.env()
	for k, v := range extra {
		if _, own := env[k]; own || k == "SCION_HUB_TOKEN" || k == "SCION_HUB_ENDPOINT" || k == "SCION_HUB_ENABLED" {
			continue
		}
		env[k] = v
	}
	res, err := c.r.RunIn(ctx, dir, env, c.bin, args...)
	out := c.filterOutput(res.Stdout) + c.filterOutput(res.Stderr)
	if err != nil {
		return "", fmt.Errorf("scion %s: %s", redactArgs(args), cleanErr(out))
	}
	return strings.TrimSpace(out), nil
}

// runStdin is run with a byte stream on stdin, for a value too big for a
// command line. The stream is never echoed: argv carries only the --config -
// marker, and scion does not repeat its stdin in an error. Runs in the
// process cwd.
func (c *Client) runStdin(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	res, err := c.r.RunStdin(ctx, stdin, c.env(), c.bin, args...)
	out := c.filterOutput(res.Stdout) + c.filterOutput(res.Stderr)
	if err != nil {
		return "", fmt.Errorf("scion %s: %s", redactArgs(args), cleanErr(out))
	}
	return strings.TrimSpace(out), nil
}

// runSecret is run for a command whose argv carries a credential. It scrubs
// the literal value from the whole error, which position-based redaction
// cannot do: a value starting with "-" parses as a flag, and cobra echoes an
// unknown flag back. Callers that pass a credential must use this.
func (c *Client) runSecret(ctx context.Context, secret string, args ...string) (string, error) {
	return c.runSecretEnv(ctx, nil, secret, args...)
}

func (c *Client) runSecretEnv(ctx context.Context, extra map[string]string, secret string, args ...string) (string, error) {
	out, err := c.runEnv(ctx, "", extra, args...)
	// Short values would scrub unrelated text; nothing lever treats as a
	// credential is that short.
	if err != nil && len(secret) >= 8 {
		return "", errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
	}
	return out, err
}

// redactArgs renders args for a user-visible error or log, masking secret
// values by position for hub secret set and hub env set --secret. It is the
// backstop under runSecret.
//
// It masks every positional after the first, and a lone KEY=VALUE positional
// as key plus value. Over-masking a separated flag value is the safe
// direction. It cannot mask a value that looks like a flag; hence runSecret.
func redactArgs(args []string) string {
	if !carriesSecret(args) {
		return strings.Join(args, " ")
	}
	out := make([]string, len(args))
	copy(out, args)

	var pos []int
	for i := 3; i < len(out); i++ {
		if !strings.HasPrefix(out[i], "-") {
			pos = append(pos, i)
		}
	}
	if len(pos) == 1 {
		if k, _, ok := strings.Cut(out[pos[0]], "="); ok {
			out[pos[0]] = k + "=***"
		} else {
			out[pos[0]] = "***"
		}
	}
	for _, i := range pos[min(len(pos), 1):] {
		out[i] = "***"
	}
	return strings.Join(out, " ")
}

func carriesSecret(args []string) bool {
	if len(args) < 4 || args[0] != "hub" || args[2] != "set" {
		return false
	}
	switch args[1] {
	case "secret":
		return true
	case "env":
		return slices.Contains(args, "--secret")
	}
	return false
}

var bannerRE = regexp.MustCompile(`(?i)WARNING:.*development auth.*`)
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)
var jsonStartRE = regexp.MustCompile(`[\[{]`)

func clean(output string) string {
	var keep []string
	for _, l := range strings.Split(output, "\n") {
		if bannerRE.MatchString(l) {
			continue
		}
		keep = append(keep, l)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// cleanErr is clean for text that becomes an error: it also masks credentials
// (RedactSecrets). clean alone runs on parsed output, which must reach a
// recipient untouched.
func cleanErr(output string) string {
	return RedactSecrets(clean(output))
}

// parseJSON skips any preamble before the first JSON token. The dev-auth
// banner can land after the JSON, since run concatenates stdout and stderr.
// An empty body is not an error.
func parseJSON(raw string, v any) error {
	body := clean(ansiRE.ReplaceAllString(raw, ""))
	loc := jsonStartRE.FindStringIndex(body)
	if loc == nil {
		return nil // nothing to parse
	}
	body = strings.TrimSpace(body[loc[0]:])
	if body == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return fmt.Errorf("could not parse scion JSON output: %w", err)
	}
	return nil
}
