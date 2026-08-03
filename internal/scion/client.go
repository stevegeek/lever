// Package scion is the single seam to the `scion` CLI. It builds argv + env and
// delegates execution to an injected exec.Runner, mirroring the Ruby ScionClient
// so every method is unit-testable with a fake runner. Anything that names a
// scion subcommand or endpoint lives HERE and nowhere else.
package scion

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/stevegeek/lever/internal/exec"
)

// DefaultHubEndpoint is the host-side hub endpoint (loopback). Passed
// explicitly as Options.HubEndpoint by host-side constructors — New applies
// no default, and an empty HubEndpoint omits SCION_HUB_ENDPOINT entirely,
// deferring to the scion binary's own default.
const DefaultHubEndpoint = "http://127.0.0.1:8080"

type Options struct {
	Bin            string        // default "scion"
	HubEndpoint    string        // SCION_HUB_ENDPOINT
	DevToken       string        // SCION_DEV_TOKEN
	HubToken       string        // SCION_HUB_TOKEN (static; the controller PAT, P3)
	HubTokenSource func() string // SCION_HUB_TOKEN (lazy; wins over HubToken — the mint-mid-apply case)
}

type Client struct {
	bin            string
	hubEndpoint    string
	devToken       string
	hubToken       string
	hubTokenSource func() string
	r              exec.Runner
}

func New(r exec.Runner, o Options) *Client {
	bin := o.Bin
	if bin == "" {
		bin = "scion"
	}
	return &Client{
		bin:            bin,
		hubEndpoint:    o.HubEndpoint,
		devToken:       o.DevToken,
		hubToken:       o.HubToken,
		hubTokenSource: o.HubTokenSource,
		r:              r,
	}
}

// currentHubToken resolves the controller PAT: the lazy source (read at call
// time, for the mint-mid-apply case where the token isn't known at New())
// wins over the static value.
func (c *Client) currentHubToken() string {
	if c.hubTokenSource != nil {
		return c.hubTokenSource()
	}
	return c.hubToken
}

// HubToken exposes the resolved controller PAT so callers outside this
// package (e.g. the attach exec path) can embed it themselves.
func (c *Client) HubToken() string { return c.currentHubToken() }

func (c *Client) env() map[string]string {
	m := map[string]string{"SCION_HUB_ENABLED": "true"}
	if c.hubEndpoint != "" {
		m["SCION_HUB_ENDPOINT"] = c.hubEndpoint
	}
	if c.devToken != "" {
		m["SCION_DEV_TOKEN"] = c.devToken
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
func (c *Client) run(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := c.r.RunIn(ctx, dir, c.env(), c.bin, args...)
	out := res.Stdout + res.Stderr
	if err != nil {
		return "", fmt.Errorf("scion %s: %s", redactArgs(args), clean(out))
	}
	return strings.TrimSpace(out), nil
}

// redactArgs renders args for a user-visible error/log, masking secret values.
// It detects the `hub secret set <KEY> <VALUE>` shape and replaces <VALUE> with
// "***" (keeping <KEY> visible). All other commands render verbatim.
func redactArgs(args []string) string {
	if len(args) == 5 && args[0] == "hub" && args[1] == "secret" && args[2] == "set" {
		redacted := make([]string, len(args))
		copy(redacted, args)
		redacted[4] = "***"
		return strings.Join(redacted, " ")
	}
	return strings.Join(args, " ")
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
