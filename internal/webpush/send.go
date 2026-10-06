package webpush

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

const (
	// SendTimeout bounds one push request, connect to answer.
	SendTimeout = 10 * time.Second
	// MaxBody is the most a push body may be (RFC 8030 §7.2 asks services
	// to take 4096 bytes).
	MaxBody = 4096
	ttl     = "3600"
)

// ErrAddress: the push host resolved to no public address.
var ErrAddress = errors.New("webpush: push service address refused")

// HTTPError is a push service's answer other than 2xx.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("webpush: push service answered HTTP %d", e.Status)
}

// Gone reports a subscription the push service no longer knows (404, 410):
// the caller deletes it.
func Gone(err error) bool {
	s := StatusOf(err)
	return s == http.StatusNotFound || s == http.StatusGone
}

// StatusOf is the HTTP status in err, or 0.
func StatusOf(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// The address ranges a push service never has: everything not global
// unicast, plus shared (the tailnet), documentation, benchmarking and
// reserved space, and the IPv6 forms that carry an IPv4 address.
var specialPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "192.88.99.0/24",
		"::/96", "::ffff:0:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32", "2002::/16", "fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() ||
		a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range specialPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// control re-checks the address a socket is about to connect to, so no path
// through the dialer reaches a refused address.
func control(test TestHosts) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		if test[address] {
			return nil
		}
		ap, err := netip.ParseAddrPort(address)
		if err != nil || ap.Port() != 443 || !publicAddr(ap.Addr()) {
			return ErrAddress
		}
		return nil
	}
}

// dialer resolves the push host itself and dials only its public
// addresses: a resolver answer of a private or tailnet address (a rebind)
// is never connected to.
type dialer struct {
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	test   TestHosts
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d *dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if d.test[addr] {
		return d.dial(ctx, "tcp4", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port != "443" {
		return nil, ErrAddress
	}
	addrs, err := d.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	last := ErrAddress
	for _, a := range addrs {
		if !publicAddr(a) {
			continue
		}
		c, err := d.dial(ctx, "tcp", netip.AddrPortFrom(a.Unmap(), 443).String())
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

// NewClient is the push client: the guarded dialer, no proxy (an HTTP_PROXY
// would make the dial check judge the proxy, not the push service), no
// redirects, SendTimeout.
func NewClient(test TestHosts) *http.Client {
	d := &dialer{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		test: test,
		dial: (&net.Dialer{Timeout: SendTimeout, Control: control(test)}).DialContext,
	}
	return &http.Client{
		Timeout: SendTimeout,
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, ForceAttemptHTTP2: true,
			TLSHandshakeTimeout: SendTimeout, ResponseHeaderTimeout: SendTimeout, MaxIdleConns: 8, IdleConnTimeout: 90 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Sender sends pushes signed with Key for Subject.
type Sender struct {
	Key     *ecdsa.PrivateKey
	Subject string
	Test    TestHosts
	Client  *http.Client // nil: NewClient(Test)
	Now     func() time.Time
}

// Send encrypts payload for sub and posts it. The endpoint is checked again
// here: a stored subscription is never trusted to still pass. An error
// names the endpoint's host, never its path (the path is the capability).
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte) error {
	u, err := CheckEndpoint(sub.Endpoint, s.Test)
	if err != nil {
		return err
	}
	p256dh, auth, err := sub.keys()
	if err != nil {
		return err
	}
	body, err := Encrypt(p256dh, auth, payload)
	if err != nil {
		return err
	}
	if len(body) > MaxBody {
		return errPayloadTooLarge
	}
	aud := "https://" + u.Hostname()
	if u.Scheme == "http" {
		aud = "http://" + u.Host
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	auth2, err := VAPIDHeader(s.Key, aud, s.Subject, now().Add(12*time.Hour))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webpush: request to %s", u.Host)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", ttl)
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Authorization", auth2)
	client := s.Client
	if client == nil {
		client = NewClient(s.Test)
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("webpush: post to %s: %w", u.Host, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return &HTTPError{Status: resp.StatusCode}
}
