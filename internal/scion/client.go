// Package scion is the single seam to the `scion` CLI. It builds argv + env and
// delegates execution to an injected exec.Runner, mirroring the Ruby ScionClient
// so every method is unit-testable with a fake runner. Anything that names a
// scion subcommand or endpoint lives HERE and nowhere else.
package scion

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lever-to/lever/internal/exec"
)

type Options struct {
	Bin         string // default "scion"
	HubEndpoint string // SCION_HUB_ENDPOINT
	DevToken    string // SCION_DEV_TOKEN
}

type Client struct {
	bin         string
	hubEndpoint string
	devToken    string
	r           exec.Runner
}

func New(r exec.Runner, o Options) *Client {
	bin := o.Bin
	if bin == "" {
		bin = "scion"
	}
	return &Client{bin: bin, hubEndpoint: o.HubEndpoint, devToken: o.DevToken, r: r}
}

// Default reads the dev token from <home>/.scion/dev-token and the hub endpoint
// from SCION_HUB_ENDPOINT (default loopback), mirroring ScionClient.default.
func Default(r exec.Runner, home string) *Client {
	token := ""
	if b, err := os.ReadFile(filepath.Join(home, ".scion", "dev-token")); err == nil {
		token = strings.TrimSpace(string(b))
	}
	endpoint := os.Getenv("SCION_HUB_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	return New(r, Options{HubEndpoint: endpoint, DevToken: token})
}

func (c *Client) env() map[string]string {
	m := map[string]string{}
	if c.hubEndpoint != "" {
		m["SCION_HUB_ENDPOINT"] = c.hubEndpoint
	}
	if c.devToken != "" {
		m["SCION_DEV_TOKEN"] = c.devToken
	}
	return m
}

func projectFlag(project string) []string {
	if project == "" {
		return nil
	}
	return []string{"-g", project}
}

// runIn executes a scion subcommand in the given working directory and returns
// trimmed combined stdout. cwd "" uses the process cwd. Non-zero exit returns
// an error with the dev-auth banner stripped for readability.
func (c *Client) runIn(ctx context.Context, dir string, args ...string) (string, error) {
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

// run executes a scion subcommand and returns trimmed combined stdout. cwd ""
// uses the process cwd. Non-zero exit returns an error with the dev-auth banner
// stripped for readability.
func (c *Client) run(ctx context.Context, cwd string, args ...string) (string, error) {
	return c.runIn(ctx, cwd, args...)
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

// parseJSON strips the ANSI dev-auth banner, skips any preamble before the first
// JSON token, and unmarshals into v. Empty body unmarshals to nothing (no error).
func parseJSON(raw string, v any) error {
	body := ansiRE.ReplaceAllString(raw, "")
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
