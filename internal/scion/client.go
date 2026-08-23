// Package scion is the single seam to the `scion` CLI. It builds argv + env and
// delegates execution to an injected exec.Runner, mirroring the Ruby ScionClient
// so every method is unit-testable with a fake runner. Anything that names a
// scion subcommand or endpoint lives HERE and nowhere else.
package scion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/exec"
)

// DefaultHubEndpoint is the hub endpoint on the loopback of whatever runs the
// request. Every caller resolves it INSIDE the jail — the scion CLI through the
// jail runner, internal/hubapi through curl — because the hub binds the jail's
// loopback and the Lima backend suppresses guest→host port forwarding by
// design. Do not treat it as host-reachable: that holds on OrbStack only, as an
// implementation detail of its port forwarding.
//
// Passed explicitly as Options.HubEndpoint by host-side constructors — New
// applies no default, and an empty HubEndpoint omits SCION_HUB_ENDPOINT
// entirely, deferring to the scion binary's own default.
var DefaultHubEndpoint = "http://127.0.0.1:" + strconv.Itoa(DefaultHubPort)

// DefaultHubPort is the port scion's hub listens on inside the jail (scion's
// own --web-port default); DefaultHubEndpoint is derived from it.
const DefaultHubPort = 8080

type Options struct {
	Bin            string        // default "scion"
	HubEndpoint    string        // SCION_HUB_ENDPOINT
	HubTokenSource func() string // SCION_HUB_TOKEN (lazy: the controller PAT is minted mid-apply)
	// AgentRole is passed as `scion start --role <role>` (scion#1089). Empty ⇒
	// the flag is omitted, which is the default: it does not exist on earlier
	// pins. See config.ScionConfig.AgentRole.
	AgentRole string
}

type Client struct {
	bin            string
	hubEndpoint    string
	hubTokenSource func() string
	agentRole      string
	r              exec.Runner

	// Poll budgets for waitHubReady / WaitRuntimeBrokerReady. New sets the
	// defaults; tests shrink them on the instance.
	hubReadyAttempts    int
	hubReadyInterval    time.Duration
	brokerReadyAttempts int
	brokerReadyInterval time.Duration
}

func New(r exec.Runner, o Options) *Client {
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

		hubReadyAttempts:    hubReadyAttempts,
		hubReadyInterval:    hubReadyInterval,
		brokerReadyAttempts: brokerReadyAttempts,
		brokerReadyInterval: brokerReadyInterval,
	}
}

// currentHubToken resolves the controller PAT from the lazy source (read at
// call time: the token isn't known at New() — it is minted mid-apply).
func (c *Client) currentHubToken() string {
	if c.hubTokenSource != nil {
		return c.hubTokenSource()
	}
	return ""
}

func (c *Client) env() map[string]string {
	m := map[string]string{"SCION_HUB_ENABLED": "true"}
	if c.hubEndpoint != "" {
		m["SCION_HUB_ENDPOINT"] = c.hubEndpoint
	}
	if tok := c.currentHubToken(); tok != "" {
		m["SCION_HUB_TOKEN"] = tok
	}
	return m
}

func projectFlag(project string) []string {
	if project == "" {
		return nil
	}
	return []string{"-g", project}
}

// run executes a scion subcommand in the given working directory and returns
// trimmed combined stdout. dir "" uses the process cwd. Non-zero exit returns
// an error with the dev-auth banner stripped for readability.
// runValue is run for a command whose STDOUT is a value lever parses, not text
// for a human. It returns stdout alone.
//
// `run` deliberately folds stderr into its result so an error message carries
// everything scion said. For a value that is wrong: scion's settings loader
// writes warnings to stderr (a legacy-format settings file, a deprecated key),
// and `run` would prepend them to the value. A caller comparing the result
// against an expected string then silently takes the wrong branch — for
// `config get default_template`, that means lever decides the operator chose
// their own template and skips the placeholder-prompt fix, while apply logs
// that it applied it. Errors still carry both streams.
func (c *Client) runValue(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := c.r.RunIn(ctx, dir, c.env(), c.bin, args...)
	if err != nil {
		return "", fmt.Errorf("scion %s: %s", redactArgs(args), clean(res.Stdout+res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (c *Client) run(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := c.r.RunIn(ctx, dir, c.env(), c.bin, args...)
	out := res.Stdout + res.Stderr
	if err != nil {
		return "", fmt.Errorf("scion %s: %s", redactArgs(args), clean(out))
	}
	return strings.TrimSpace(out), nil
}

// runSecret is run for a command whose argv carries a credential. It scrubs the
// literal value from the whole error — argv and scion's own output alike —
// which position-based redaction cannot do: a value starting with "-" parses as
// a flag, and cobra echoes an unknown flag back in its error text.
// Callers that pass a credential MUST use this rather than run.
func (c *Client) runSecret(ctx context.Context, dir, secret string, args ...string) (string, error) {
	out, err := c.run(ctx, dir, args...)
	// Short values would scrub unrelated text; nothing lever treats as a
	// credential is that short.
	if err != nil && len(secret) >= 8 {
		return "", errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
	}
	return out, err
}

// redactArgs renders args for a user-visible error/log, masking secret values by
// position. It is the backstop under runSecret, and the only defence for a
// caller that forgets it. Two shapes carry a secret: `hub secret set` and the
// `--secret` form of `hub env set` (which writes the same Hub row). All other
// commands render verbatim.
//
// It masks every positional after the first, and treats a lone KEY=VALUE
// positional as key plus value. That over-masks a command with a separated flag
// value (`--type file`), which is the safe direction: a missed mask puts a
// credential in an error message. It cannot mask a value that looks like a
// flag — hence runSecret.
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

// parseJSON strips ANSI escapes and the dev-auth WARNING banner (which scion
// prints on stderr and can land AFTER the JSON, since run concatenates
// stdout+stderr), skips any preamble before the first JSON token, and unmarshals
// into v. Empty body unmarshals to nothing (no error).
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
