package hubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
)

// AssertionHeader is the request header the hub's proxy auth reads the lever
// assertion from.
const AssertionHeader = "X-Lever-Hub-Assertion"

// assertCurlScript sends the assertion as a header that curl reads from
// stdin (-H @-), so the value is in neither the host nor the guest argv;
// printf is a shell builtin, so it does not exec with the value either.
//
// The header line ends in a bare \n: curl strips only the \n, so a \r\n line
// would send a stray CR inside the header value.
//
// -q skips any .curlrc and --noproxy '*' ignores proxy variables in the guest
// environment: either could otherwise add headers or route an admin-grade
// credential through another host. No Authorization header is ever sent: a
// bearer takes precedence over the assertion on the hub.
const assertCurlScript = `[ -n "$` + jail.HubAssertionEnv + `" ] || exit 1; ` +
	`printf '` + AssertionHeader + `: %s\n' "$` + jail.HubAssertionEnv + `" | ` +
	`exec curl -q -sS --noproxy '*' --connect-timeout 5 --max-time 20 ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H @- -H 'Accept: application/json' "$2"`

// assertCurlBodyScript is assertCurlScript with a JSON body from the
// environment, like curlBodyScript.
const assertCurlBodyScript = `[ -n "$` + jail.HubAssertionEnv + `" ] || exit 1; ` +
	`printf '` + AssertionHeader + `: %s\n' "$` + jail.HubAssertionEnv + `" | ` +
	`exec curl -q -sS --noproxy '*' --connect-timeout 5 --max-time 20 ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H @- -H 'Accept: application/json' ` +
	`-H 'Content-Type: application/json' --data-binary "$LEVER_HUB_BODY" "$2"`

// AssertCurl is a Doer that runs one request inside the jail with curl,
// authenticated by a fresh lever assertion instead of the controller PAT.
type AssertCurl struct {
	Runner  proc.Runner
	BaseURL string
	// Sign returns a fresh assertion for this identity; called once per
	// request, and only for a path AssertionPathAllowed accepts.
	Sign func() (string, error)
}

// ErrAssertionPathRefused means AssertCurl would not sign for the request.
var ErrAssertionPathRefused = errors.New("refusing to send a hub assertion")

// assertionPrefixes are the hub routes an assertion may reach. Everything
// else is refused: scion's agent port proxy forwards request headers into the
// agent, so an assertion sent there would hand the agent a hub credential.
var assertionPrefixes = []string{
	"/api/v1/admin/",
	"/api/v1/users",
	"/api/v1/auth/me",
	"/api/v1/auth/tokens",
	"/api/v1/projects/register",
	"/api/v1/system/identity",
	"/api/v1/runtime-brokers",
}

// assertionReadOnlyPrefix is reachable with GET only: it holds the project
// lookups the identity steps need, and every project mutation stays with the
// PAT.
const assertionReadOnlyPrefix = "/api/v1/projects"

var assertionMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true,
}

func underPrefix(p, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(p, prefix)
	}
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// AssertionPathAllowed reports whether AssertCurl may sign an assertion for
// method and path (a hub path with an optional query). It is deliberately
// stricter than the hub's router: a path the router would clean, decode or
// redirect is refused rather than reasoned about.
func AssertionPathAllowed(method, path string) error {
	refuse := func(why string) error {
		return fmt.Errorf("%w for %s %q: %s", ErrAssertionPathRefused, method, path, why)
	}
	if !assertionMethods[method] {
		return refuse("method not allowed")
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return refuse("control character or space")
		}
	}
	if strings.ContainsAny(path, "\\#") {
		return refuse("backslash or fragment")
	}
	p, _, _ := strings.Cut(path, "?")
	if !strings.HasPrefix(p, "/") {
		return refuse("not an absolute path")
	}
	if strings.Contains(p, "..") || strings.Contains(p, "//") {
		return refuse("traversal or empty segment")
	}
	// Covers %2e, %2f and %5c in any case, and every other escape the router
	// would decode before matching; no allowed route needs one.
	if strings.Contains(p, "%") {
		return refuse("percent-encoded path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || strings.EqualFold(seg, "ports") {
			return refuse("dot or ports segment")
		}
	}
	for _, prefix := range assertionPrefixes {
		if underPrefix(p, prefix) {
			return nil
		}
	}
	if underPrefix(p, assertionReadOnlyPrefix) {
		if method == http.MethodGet {
			return nil
		}
		return refuse("projects routes are read-only")
	}
	return refuse("path not on the assertion allowlist")
}

func (a *AssertCurl) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return a.run(ctx, method, path, nil)
}

// DoBody is Do with a JSON request body; it makes AssertCurl a BodyDoer.
func (a *AssertCurl) DoBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	return a.run(ctx, method, path, body)
}

func (a *AssertCurl) run(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if err := AssertionPathAllowed(method, path); err != nil {
		return 0, nil, err
	}
	// curl reads --data-binary @x as a file name.
	if len(body) > 0 && body[0] == '@' {
		return 0, nil, fmt.Errorf("%s %s: request body starts with @", method, path)
	}
	if a.Sign == nil {
		return 0, nil, fmt.Errorf("no assertion signer for %s %s", method, path)
	}
	assertion, err := a.Sign()
	if err != nil {
		return 0, nil, fmt.Errorf("signing a hub assertion for %s %s: %w", method, path, err)
	}
	if assertion == "" {
		return 0, nil, fmt.Errorf("signing a hub assertion for %s %s: empty assertion", method, path)
	}

	env := map[string]string{jail.HubAssertionEnv: assertion}
	script := assertCurlScript
	if body != nil {
		env["LEVER_HUB_BODY"] = string(body)
		script = assertCurlBodyScript
	}
	return runCurl(ctx, a.Runner, env, script, method, path, strings.TrimRight(a.BaseURL, "/")+path)
}
