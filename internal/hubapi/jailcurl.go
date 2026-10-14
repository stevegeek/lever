package hubapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/stevegeek/lever/internal/proc"
)

// curlNotFound is the shell's exit code for an absent command. Guest
// provisioning installs curl, so this means the jail is not provisioned, not
// that the hub is down.
const curlNotFound = 127

var errCurlMissing = errors.New("curl is missing from the jail")

// curlFileSizeExceeded is curl's exit code when --max-filesize stops a
// transfer.
const curlFileSizeExceeded = 63

// DefaultReplyCap bounds a hub reply body when the call sets no cap of its
// own: a wrong service on the hub port, or a record carrying agent-written
// text, is not bounded by anything else.
const DefaultReplyCap = 1 << 20

// statusTrailerRoom is the output allowed past the body cap for the status
// line curl's -w appends.
const statusTrailerRoom = 64

type replyCapKey struct{}

// WithReplyCap returns ctx carrying the largest reply body, in bytes, that
// JailCurl and AssertCurl accept for calls made with it. A larger reply is an
// error, never a truncated body.
func WithReplyCap(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, replyCapKey{}, n)
}

// ReplyCap is the reply cap WithReplyCap set on ctx, or DefaultReplyCap.
func ReplyCap(ctx context.Context) int {
	if n, ok := ctx.Value(replyCapKey{}).(int); ok && n > 0 {
		return n
	}
	return DefaultReplyCap
}

// curlScript runs one request from inside the jail. The Authorization header is
// built from $SCION_HUB_TOKEN by the in-guest shell; the token reaches the jail
// through the runner's env, and the jail runner keeps that key off the host
// command line, so it is never visible to ps on the host.
//
// -w appends a final line holding the status code; curl expands the \n
// itself. --max-filesize takes the reply cap from $3; it stops a transfer
// whose Content-Length or received size passes it, and the runner's output
// limit backs it for a curl that only checks Content-Length. --fail is deliberately not set, so a 403 or 404 arrives as a status
// rather than a non-zero exit indistinguishable from an unreachable hub.
const curlScript = `exec curl -sS --connect-timeout 5 --max-time 20 --max-filesize "$3" ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H "Authorization: Bearer $SCION_HUB_TOKEN" -H 'Accept: application/json' "$2"`

// curlBodyScript is curlScript for a request with a JSON body. The body rides
// the environment so lever never quotes JSON into script text.
const curlBodyScript = `exec curl -sS --connect-timeout 5 --max-time 20 --max-filesize "$3" ` +
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
	return runCurlStdin(ctx, r, nil, env, script, method, path, url)
}

// runCurlStdin is runCurl with stdin for the script.
func runCurlStdin(ctx context.Context, r proc.Runner, stdin io.Reader, env map[string]string, script, method, path, url string) (int, []byte, error) {
	limit := ReplyCap(ctx)
	ctx = proc.WithOutputLimit(ctx, limit+statusTrailerRoom)
	args := []string{"-c", script, "_", method, url, strconv.Itoa(limit)}
	var res proc.Result
	var err error
	if stdin == nil {
		res, err = r.Run(ctx, env, "sh", args...)
	} else {
		res, err = r.RunStdin(ctx, stdin, env, "sh", args...)
	}
	return curlResult(res, err, method, path, limit)
}

// replyTooLarge is an APIError: something answered, just with more than lever
// reads for this call.
func replyTooLarge(method, path string, limit int) error {
	return &APIError{TooLarge: true, Msg: fmt.Sprintf(
		"%s %s: the hub's reply passed the %s lever accepts for this call; refusing a partial reply",
		method, path, byteSize(limit))}
}

// byteSize renders n bytes for an operator.
func byteSize(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func curlResult(res proc.Result, err error, method, path string, limit int) (int, []byte, error) {
	if err != nil {
		// Checked first: a killed or stopped transfer may still have written
		// what looks like a whole reply and a status line.
		if errors.Is(err, proc.ErrOutputLimit) || res.Code == curlFileSizeExceeded {
			return 0, nil, replyTooLarge(method, path, limit)
		}
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
	// Say "too large" rather than truncate: a cut body surfaces as a JSON
	// decode error that points at the wrong cause.
	if len(body) > limit {
		return 0, nil, replyTooLarge(method, path, limit)
	}
	return status, bytes.TrimSpace(body), nil
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
	return []byte(out[:i]), code, nil
}
