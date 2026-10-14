package hubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// AssertionHeader is the request header the hub's proxy auth reads the lever
// assertion from.
const AssertionHeader = "X-Lever-Hub-Assertion"

// assertCurlScript reads the assertion from the first line of stdin into an
// unexported shell variable and hands it to curl as a header read from stdin
// (-H @-), so the value is in no argv and in no environment, host or guest:
// nothing else running as the jail user can read it from /proc. printf is a
// shell builtin, so it does not exec with the value either.
//
// The header line ends in a bare \n: curl strips only the \n, so a \r\n line
// would send a stray CR inside the header value.
//
// -g turns off curl's URL globbing, which would turn one signed request into
// several. -q skips any .curlrc and --noproxy '*' ignores proxy variables in the guest
// environment: either could otherwise add headers or route an admin-grade
// credential through another host. No Authorization header is ever sent: a
// bearer takes precedence over the assertion on the hub.
const assertCurlScript = `IFS= read -r t || exit 1; [ -n "$t" ] || exit 1; ` +
	`printf '` + AssertionHeader + `: %s\n' "$t" | ` +
	`exec curl -q -g -sS --noproxy '*' --connect-timeout 5 --max-time 20 --max-filesize "$3" ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H @- -H 'Accept: application/json' "$2"`

// assertCurlBodyScript is assertCurlScript with a JSON body from the
// environment, like curlBodyScript.
const assertCurlBodyScript = `IFS= read -r t || exit 1; [ -n "$t" ] || exit 1; ` +
	`printf '` + AssertionHeader + `: %s\n' "$t" | ` +
	`exec curl -q -g -sS --noproxy '*' --connect-timeout 5 --max-time 20 --max-filesize "$3" ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H @- -H 'Accept: application/json' ` +
	`-H 'Content-Type: application/json' --data-binary "$LEVER_HUB_BODY" "$2"`

// assertCurlIfMatchScript is assertCurlBodyScript with an If-Match header
// from the environment. run accepts only a decimal revision, so the value
// cannot carry a header break.
const assertCurlIfMatchScript = `IFS= read -r t || exit 1; [ -n "$t" ] || exit 1; ` +
	`printf '` + AssertionHeader + `: %s\n' "$t" | ` +
	`exec curl -q -g -sS --noproxy '*' --connect-timeout 5 --max-time 20 --max-filesize "$3" ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H @- -H 'Accept: application/json' -H "If-Match: $LEVER_HUB_IF_MATCH" ` +
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

// assertionRoute is one hub call lever makes with an assertion. A pattern
// segment "{id}" matches one hub-issued ID; query says whether the call
// carries a query string.
type assertionRoute struct {
	method, pattern string
	query           bool
}

// assertionRoutes are exactly the calls lever signs for, by method and path.
// Everything else is refused: scion's agent port proxy forwards request
// headers into the agent, so an assertion sent there would hand the agent a
// hub credential, and an admin-grade assertion has no business on a route
// lever does not use.
var assertionRoutes = []assertionRoute{
	{http.MethodGet, "/api/v1/auth/me", false},
	{http.MethodGet, "/api/v1/users", true},
	{http.MethodGet, "/api/v1/users/{id}", false},
	{http.MethodPatch, "/api/v1/users/{id}", false},
	{http.MethodPost, "/api/v1/admin/users/invite", false},
	{http.MethodGet, "/api/v1/admin/roles", false},
	{http.MethodPost, "/api/v1/admin/roles", false},
	{http.MethodPut, "/api/v1/admin/roles/{id}", false},
	{http.MethodGet, "/api/v1/admin/role-bindings", true},
	{http.MethodPost, "/api/v1/admin/role-bindings", false},
	{http.MethodDelete, "/api/v1/admin/role-bindings/{id}", false},
	{http.MethodGet, "/api/v1/admin/access-constraints", true},
	{http.MethodGet, "/api/v1/admin/access-constraints/{id}", false},
	{http.MethodPost, "/api/v1/admin/access-constraints", false},
	{http.MethodPut, "/api/v1/admin/access-constraints/{id}", false},
	{http.MethodDelete, "/api/v1/admin/access-constraints/{id}", false},
	{http.MethodPost, "/api/v1/admin/access-constraint-previews", false},
	{http.MethodGet, "/api/v1/auth/tokens", false},
	{http.MethodPost, "/api/v1/auth/tokens", false},
	{http.MethodPost, "/api/v1/auth/tokens/{id}/revoke", false},
	{http.MethodGet, "/api/v1/projects", false},
	{http.MethodPost, "/api/v1/projects/register", false},
	{http.MethodGet, "/api/v1/runtime-brokers", false},
	{http.MethodPost, "/api/v1/projects/{id}/providers", false},
	{http.MethodGet, "/api/v1/admin/experiments", false},
	{http.MethodPut, "/api/v1/admin/experiments", false},
	{http.MethodGet, "/api/v1/conduit/grant-keys", false},
	{http.MethodGet, "/api/v1/artifacts", true},
	{http.MethodGet, "/api/v1/thread-groups", false},
	{http.MethodPut, "/api/v1/system/identity", false},
	{http.MethodPut, "/api/v1/system/registry", false},
	{http.MethodGet, "/api/v1/system/runtime", false},
	{http.MethodPut, "/api/v1/system/runtime", false},
}

// idSegment is what a hub-issued ID may look like in a signed path: scion's
// IDs are UUIDs, so this is generous and still excludes every separator.
func idSegment(seg string) bool {
	if seg == "" || len(seg) > 128 {
		return false
	}
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func (rt assertionRoute) matches(method, p string, hasQuery bool) bool {
	if method != rt.method || (hasQuery && !rt.query) {
		return false
	}
	want, got := strings.Split(rt.pattern, "/"), strings.Split(p, "/")
	if len(want) != len(got) {
		return false
	}
	for i, w := range want {
		if w == "{id}" {
			if !idSegment(got[i]) {
				return false
			}
		} else if got[i] != w {
			return false
		}
	}
	return true
}

// AssertionPathAllowed reports whether AssertCurl may sign an assertion for
// method and path (a hub path with an optional query). It is deliberately
// stricter than the hub's router: a path the router would clean, decode or
// redirect is refused rather than reasoned about.
func AssertionPathAllowed(method, path string) error {
	refuse := func(why string) error {
		return fmt.Errorf("%w for %s %q: %s", ErrAssertionPathRefused, method, path, why)
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
	q := strings.Contains(path, "?")
	for _, rt := range assertionRoutes {
		if rt.matches(method, p, q) {
			return nil
		}
	}
	return refuse("not a call lever signs for")
}

func (a *AssertCurl) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return a.run(ctx, method, path, nil)
}

// DoBody is Do with a JSON request body; it makes AssertCurl a BodyDoer.
func (a *AssertCurl) DoBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	return a.run(ctx, method, path, body)
}

// DoBodyIfMatch is DoBody with an If-Match header; it makes AssertCurl an
// IfMatchDoer. ifMatch must be a decimal revision.
func (a *AssertCurl) DoBodyIfMatch(ctx context.Context, method, path string, body []byte, ifMatch string) (int, []byte, error) {
	if !decimalRevision(ifMatch) {
		return 0, nil, fmt.Errorf("%s %s: If-Match %q is not a decimal revision", method, path, ifMatch)
	}
	return a.run(ctx, method, path, body, ifMatch)
}

func decimalRevision(s string) bool {
	if s == "" || len(s) > 19 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (a *AssertCurl) run(ctx context.Context, method, path string, body []byte, ifMatch ...string) (int, []byte, error) {
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

	if strings.ContainsAny(assertion, "\r\n") {
		return 0, nil, fmt.Errorf("signing a hub assertion for %s %s: the assertion has a line break", method, path)
	}
	var env map[string]string
	script := assertCurlScript
	if body != nil {
		env = map[string]string{"LEVER_HUB_BODY": string(body)}
		script = assertCurlBodyScript
	}
	if len(ifMatch) > 0 {
		if body == nil {
			return 0, nil, fmt.Errorf("%s %s: If-Match needs a request body", method, path)
		}
		env["LEVER_HUB_IF_MATCH"] = ifMatch[0]
		script = assertCurlIfMatchScript
	}
	return runCurlStdin(ctx, a.Runner, strings.NewReader(assertion+"\n"), env, script, method, path, strings.TrimRight(a.BaseURL, "/")+path)
}
