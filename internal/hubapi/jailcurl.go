package hubapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// curlNotFound is the shell's exit code for an absent command. Guest
// provisioning installs curl, so this means the jail is not provisioned, not
// that the hub is down.
const curlNotFound = 127

var errCurlMissing = errors.New("curl is missing from the jail")

// maxBody caps the response read: a wrong service on the hub port is not
// bounded by anything else.
const maxBody = 64 << 10

// curlScript runs one request from inside the jail. The Authorization header is
// built from $SCION_HUB_TOKEN by the in-guest shell; the token reaches the jail
// through the runner's env, and the jail runner keeps that key off the host
// command line, so it is never visible to ps on the host.
//
// -w appends a final line holding the status code; curl expands the \n
// itself. --fail is deliberately not set, so a 403 or 404 arrives as a status
// rather than a non-zero exit indistinguishable from an unreachable hub.
const curlScript = `exec curl -sS --connect-timeout 5 --max-time 20 ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H "Authorization: Bearer $SCION_HUB_TOKEN" -H 'Accept: application/json' "$2"`

// curlBodyScript is curlScript for a request with a JSON body. The body rides
// the environment so lever never quotes JSON into script text.
const curlBodyScript = `exec curl -sS --connect-timeout 5 --max-time 20 ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H "Authorization: Bearer $SCION_HUB_TOKEN" -H 'Accept: application/json' ` +
	`-H 'Content-Type: application/json' --data-binary "$LEVER_HUB_BODY" "$2"`

// JailCurl is a Doer that runs each request inside the jail with curl. The
// host cannot reach the hub: the Lima template suppresses every guest-to-host
// port forward so a jailed agent cannot squat a host loopback port.
type JailCurl struct {
	// Runner executes inside the jail.
	Runner proc.Runner
	// BaseURL is the hub root as seen from the jail.
	BaseURL string
	// Token returns the controller PAT. Called per request so a re-mint is
	// picked up.
	Token func() string
}

func (j *JailCurl) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return j.run(ctx, method, path, nil)
}

// DoBody is Do with a JSON request body; it makes JailCurl a BodyDoer.
func (j *JailCurl) DoBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	return j.run(ctx, method, path, body)
}

func (j *JailCurl) run(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	tok := ""
	if j.Token != nil {
		tok = j.Token()
	}
	if tok == "" {
		return 0, nil, fmt.Errorf("no controller PAT available for %s %s", method, path)
	}

	env := map[string]string{"SCION_HUB_TOKEN": tok}
	script := curlScript
	if body != nil {
		env["LEVER_HUB_BODY"] = string(body)
		script = curlBodyScript
	}
	return runCurl(ctx, j.Runner, env, script, method, path, strings.TrimRight(j.BaseURL, "/")+path)
}

func runCurl(ctx context.Context, r proc.Runner, env map[string]string, script, method, path, url string) (int, []byte, error) {
	res, err := r.Run(ctx, env, "sh", "-c", script, "_", method, url)
	if err != nil {
		if res.Code == curlNotFound {
			return 0, nil, fmt.Errorf("%s %s: %w (is it provisioned?): %s",
				method, path, errCurlMissing, strings.TrimSpace(res.Stderr))
		}
		return 0, nil, fmt.Errorf("%s %s: reaching the hub from the jail: %w: %s",
			method, path, err, strings.TrimSpace(res.Stderr))
	}

	body, status, perr := splitCurlOutput(res.Stdout)
	if perr != nil {
		// curl exited 0, so something answered, just not like the hub: an
		// APIError, not a transport failure.
		return 0, nil, &APIError{Msg: fmt.Sprintf("%s %s: %v", method, path, perr)}
	}
	return status, body, nil
}

// splitCurlOutput returns an error rather than a zero status when the tail is
// not a status code, so a truncated or hijacked response never reads as a
// successful request.
func splitCurlOutput(out string) ([]byte, int, error) {
	i := strings.LastIndexByte(out, '\n')
	if i < 0 {
		return nil, 0, fmt.Errorf("curl wrote no status line: %q", snippet([]byte(out)))
	}
	code, err := strconv.Atoi(strings.TrimSpace(out[i+1:]))
	if err != nil {
		return nil, 0, fmt.Errorf("curl wrote no status line: %q", snippet([]byte(out)))
	}
	body := []byte(out[:i])
	// Say "too large" rather than truncate: a cut body surfaces as a JSON
	// decode error that points at the wrong cause.
	if len(body) > maxBody {
		return nil, 0, fmt.Errorf("response body exceeds %d bytes (is something other than the hub on this port?)", maxBody)
	}
	return bytes.TrimSpace(body), code, nil
}
