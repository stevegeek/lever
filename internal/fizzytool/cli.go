package fizzytool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

// CLI runs the fizzy CLI: argv only, an empty cwd and HOME, the token in
// FIZZY_TOKEN (never --token, which /proc would show), the API URL pinned in
// FIZZY_API_URL (the CLI walks UP from cwd for a .fizzy.yaml, which could
// otherwise redirect the token to another host — CheckNoLocalConfig refuses
// that at startup too), JSON output, a timeout and an output cap.
type CLI struct {
	Bin, Account, Token, Home, Work, APIURL string
	Timeout                                 time.Duration
	MaxOut                                  int
}

// CheckNoLocalConfig refuses a work dir with a fizzy config file in it or in
// any ancestor: the CLI reads the nearest one walking up from cwd.
func CheckNoLocalConfig(work string) error {
	// The child's cwd resolves to the real path and the CLI walks the real
	// ancestors, so check those, not the lexical ones.
	abs, err := filepath.Abs(work)
	if err != nil {
		return fmt.Errorf("fizzy: resolve work dir %s: %w", work, err)
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("fizzy: resolve work dir %s: %w", work, err)
	}
	for {
		for _, n := range []string{".fizzy.yaml", ".fizzy.yml"} {
			p := filepath.Join(dir, n)
			_, err := os.Lstat(p)
			if err == nil {
				return fmt.Errorf("fizzy: %s exists; the CLI would read it from %s — remove it or move -state elsewhere", p, work)
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("fizzy: cannot check %s: %w", p, err)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// CheckVersion requires fizzy CLI major version 4 (the flags and envelope
// this package relies on; FIZZY_ACCOUNT is deprecated in 4.x).
func CheckVersion(ctx context.Context, c CLI) error {
	data, err := c.Run(ctx, "version")
	if err != nil {
		return err
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &v) != nil || !strings.HasPrefix(v.Version, "4.") {
		return fmt.Errorf("fizzy CLI version %q unsupported: need 4.x", v.Version)
	}
	return nil
}

type capWriter struct {
	buf bytes.Buffer
	max int
	hit bool
}

// Write keeps at most max bytes but always reports len(p) consumed, so
// os/exec keeps draining the pipe and the child is not blocked until the
// timeout.
func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.buf.Len()+len(p) > w.max {
		w.hit = true
		p = p[:max(0, w.max-w.buf.Len())]
	}
	w.buf.Write(p)
	return n, nil
}

func (c CLI) scrub(s string) string {
	if c.Token != "" {
		s = strings.ReplaceAll(s, c.Token, "[REDACTED]")
	}
	return scion.RedactSecrets(s)
}

// Run executes `fizzy <args> --agent --json` and returns the envelope's data.
func (c CLI) Run(ctx context.Context, args ...string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, append(append([]string{}, args...), "--agent", "--json")...)
	cmd.Dir = c.Work
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + c.Home, "FIZZY_TOKEN=" + c.Token, "FIZZY_ACCOUNT=" + c.Account, "FIZZY_API_URL=" + c.APIURL}
	out := &capWriter{max: c.MaxOut}
	errOut := &capWriter{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, errOut
	runErr := cmd.Run()
	if out.hit {
		return nil, fmt.Errorf("fizzy %s: output larger than %d bytes", args[0], c.MaxOut)
	}
	var env struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error any             `json:"error"`
	}
	if err := json.Unmarshal(out.buf.Bytes(), &env); err != nil || !env.OK || runErr != nil {
		detail := strings.TrimSpace(errOut.buf.String())
		if env.Error != nil {
			b, _ := json.Marshal(env.Error)
			detail = string(b) + " " + detail
		}
		if runErr != nil {
			detail = runErr.Error() + ": " + detail
		}
		return nil, fmt.Errorf("fizzy %s: %s", strings.Join(args[:min(2, len(args))], " "), c.scrub(strings.TrimSpace(detail)))
	}
	return env.Data, nil
}
