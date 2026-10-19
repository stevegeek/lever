package servicehost

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Transport dials a service host with the broker's client key and accepts
// only the pinned server. Keep-alives are off: a connection left over from
// before the host slept or restarted could hang a call, and a fresh
// handshake per call is what makes the next call work once the host is back.
func Transport(client tls.Certificate, serverPin Fingerprint) *http.Transport {
	return transport(ClientTLSConfig(client, serverPin))
}

// FileTransport is Transport with the client pair read from disk, again
// whenever either file changes, so a re-pair takes effect on the next call.
func FileTransport(certPath, keyPath string, serverPin Fingerprint) *http.Transport {
	f := &fileCert{certPath: certPath, keyPath: keyPath}
	return transport(clientTLSConfig(f.load, serverPin))
}

func transport(tc *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:     tc,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
}

// ToolURL is the URL the broker forwards tool name to.
func ToolURL(address, name string) string {
	return "https://" + address + ToolPath(name)
}

// ProbeError says which step of reaching a service host failed.
type ProbeError struct {
	Step string // "unreachable", "pin", "refused", "answer"
	Err  error
}

func (e *ProbeError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *ProbeError) Unwrap() error { return e.Err }

// Probe completes the mutual TLS handshake with address and reads the
// service host's Info.
func Probe(ctx context.Context, address string, client tls.Certificate, serverPin Fingerprint) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tr := Transport(client, serverPin)
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+InfoPath, nil)
	if err != nil {
		return Info{}, &ProbeError{"unreachable", err}
	}
	resp, err := (&http.Client{Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return Info{}, classify(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, &ProbeError{"answer", fmt.Errorf("HTTP %d", resp.StatusCode)}
	}
	var info Info
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return Info{}, &ProbeError{"answer", err}
	}
	return info, nil
}

// classify tells a pin mismatch and a refused client key from a host that
// cannot be reached. Under TLS 1.3 the server's refusal of the client key
// arrives as an alert after the client's side of the handshake is done.
func classify(err error) error {
	switch {
	case errors.Is(err, ErrPinMismatch):
		return &ProbeError{"pin", err}
	case strings.Contains(err.Error(), "remote error: tls:"):
		return &ProbeError{"refused", err}
	default:
		return &ProbeError{"unreachable", err}
	}
}

type fileCert struct {
	certPath, keyPath string

	mu    sync.Mutex
	stamp string
	cert  *tls.Certificate
}

func (f *fileCert) load() (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stamp, err := fileStamp(f.certPath, f.keyPath)
	if err != nil {
		return nil, err
	}
	if f.cert != nil && stamp == f.stamp {
		return f.cert, nil
	}
	cert, _, err := LoadClientKeyPair(f.certPath, f.keyPath)
	if err != nil {
		return nil, err
	}
	f.cert, f.stamp = &cert, stamp
	return f.cert, nil
}

func fileStamp(paths ...string) (string, error) {
	var b strings.Builder
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%d/%d/%o;", fi.ModTime().UnixNano(), fi.Size(), fi.Mode().Perm())
	}
	return b.String(), nil
}
