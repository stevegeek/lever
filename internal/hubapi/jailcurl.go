package hubapi

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	leverexec "github.com/stevegeek/lever/internal/exec"
)

// curlNotFound is the shell's exit code for an absent command. lever's guest
// provisioning installs curl (internal/backend/guest/guest.go), so this means
// the jail is not provisioned, not that the hub is down — worth saying so.
const curlNotFound = 127

// maxBody caps the response lever reads. The hub's replies here are small; a
// wrong service on the hub port is not bounded by anything else.
const maxBody = 64 << 10

// curlScript runs one request from inside the jail. The Authorization header is
// built HERE, from $SCION_HUB_TOKEN, so the shell expands it: a token embedded
// in an argument value would not be re-expanded. The token reaches the jail
// through the runner's `env K=V` prefix, exactly as every scion CLI call
// already passes it (internal/scion/lifecycle.go) — the same exposure, not a
// wider one.
//
// -o - writes the body to stdout and -w appends a final line holding the status
// code, so one stream carries both. curl expands the `\n` itself (verified live
// against the jail). --fail is deliberately NOT set: curl exits 0 on a 4xx
// without it, which is what lever wants — a 403 or 404 must arrive as a status
// to act on, not as a bare non-zero exit indistinguishable from an unreachable
// hub.
const curlScript = `exec curl -sS --connect-timeout 5 --max-time 20 ` +
	`-o - -w '\n%{http_code}' -X "$1" ` +
	`-H "Authorization: Bearer $SCION_HUB_TOKEN" -H 'Accept: application/json' "$2"`

// JailCurl is a Doer that runs each request inside the jail with curl.
//
// The hub binds the jail's loopback. Lever's Lima template suppresses every
// guest→host port forward on purpose (a jailed agent must not be able to squat
// a host loopback port), so the host cannot reach the hub on that backend at
// all. Running in the jail is also the only way to address the right hub when
// two lever instances are up: each has its own 127.0.0.1:8080.
type JailCurl struct {
	// Runner executes inside the jail (internal/jail.Runner).
	Runner leverexec.Runner
	// BaseURL is the hub root AS SEEN FROM THE JAIL, e.g. "http://127.0.0.1:8080".
	BaseURL string
	// Token returns the controller PAT. Called per request so a re-mint between
	// calls is picked up without rebuilding the transport.
	Token func() string
}

func (j *JailCurl) Do(ctx context.Context, method, path string) (int, []byte, error) {
	tok := ""
	if j.Token != nil {
		tok = j.Token()
	}
	if tok == "" {
		return 0, nil, fmt.Errorf("no controller PAT available for %s %s", method, path)
	}

	url := strings.TrimRight(j.BaseURL, "/") + path
	res, err := j.Runner.Run(ctx, map[string]string{"SCION_HUB_TOKEN": tok},
		"sh", "-c", curlScript, "_", method, url)
	if err != nil {
		if res.Code == curlNotFound {
			return 0, nil, fmt.Errorf("%s %s: curl is missing from the jail (is it provisioned?): %s",
				method, path, strings.TrimSpace(res.Stderr))
		}
		return 0, nil, fmt.Errorf("%s %s: reaching the hub from the jail: %w: %s",
			method, path, err, strings.TrimSpace(res.Stderr))
	}

	body, status, perr := splitCurlOutput(res.Stdout)
	if perr != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, perr)
	}
	return status, body, nil
}

// splitCurlOutput separates the response body from the trailing status-code
// line that -w appends. It returns an error rather than a zero status when the
// tail is not a status code, so a truncated or hijacked response can never read
// as a successful request.
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
	// Say "too large" rather than truncate. A silently cut body reaches the
	// caller as "decoding response: unexpected end of JSON input", which points
	// at the wrong cause — and the doctor would render that as a red line
	// blaming the hub's answer.
	if len(body) > maxBody {
		return nil, 0, fmt.Errorf("response body exceeds %d bytes (is something other than the hub on this port?)", maxBody)
	}
	return bytes.TrimSpace(body), code, nil
}
