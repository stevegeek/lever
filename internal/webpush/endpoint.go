package webpush

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

var (
	// ErrEndpoint: not an endpoint of a known push service (the SSRF guard).
	ErrEndpoint = errors.New("webpush: endpoint is not a known push service")
	// ErrKeys: the subscription's p256dh or auth is malformed.
	ErrKeys = errors.New("webpush: subscription keys are malformed")
)

// Subscription is one browser's push subscription, as PushSubscription.toJSON()
// gives it, keys flattened.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256DH   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

const maxEndpoint = 1024

// The push services Chrome, Safari (web.push.apple.com), Firefox and Edge
// hand out endpoints on. A suffix needs at least one label before it.
var (
	pushHosts    = []string{"fcm.googleapis.com", "updates.push.services.mozilla.com"}
	pushSuffixes = []string{".push.apple.com", ".notify.windows.com"}
)

// TestHostsEnv names the test-only exception: loopback addresses the e2e's
// fake push service listens on. Never set it for a real instance.
const TestHostsEnv = "LEVER_PUSH_TEST_HOSTS"

// TestHosts are exact "127.0.0.1:<port>" addresses admitted, over plain
// http, by CheckEndpoint and the dialer. Empty in production.
type TestHosts map[string]bool

// ParseTestHosts reads TestHostsEnv. Anything but a list of
// 127.0.0.1:<port> is an error: the proxy refuses to start rather than
// widen the allow-list by mistake.
func ParseTestHosts(v string) (TestHosts, error) {
	out := TestHosts{}
	if v == "" {
		return out, nil
	}
	for _, h := range strings.Split(v, ",") {
		ap, err := netip.ParseAddrPort(h)
		if err != nil || ap.Addr() != netip.AddrFrom4([4]byte{127, 0, 0, 1}) || ap.Port() == 0 || ap.String() != h {
			return nil, fmt.Errorf("webpush: %s: %q is not a 127.0.0.1:<port> address", TestHostsEnv, h)
		}
		out[h] = true
	}
	return out, nil
}

// CheckEndpoint returns the parsed endpoint when it is one of a known push
// service (or a test host), else ErrEndpoint.
func CheckEndpoint(endpoint string, test TestHosts) (*url.URL, error) {
	if endpoint == "" || len(endpoint) > maxEndpoint {
		return nil, ErrEndpoint
	}
	for _, c := range []byte(endpoint) {
		if c <= ' ' || c >= 0x7f {
			return nil, ErrEndpoint
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" ||
		!strings.HasPrefix(u.EscapedPath(), "/") {
		return nil, ErrEndpoint
	}
	if test[u.Host] {
		if u.Scheme != "http" {
			return nil, ErrEndpoint
		}
		return u, nil
	}
	if u.Scheme != "https" || strings.HasPrefix(u.Host, "[") {
		return nil, ErrEndpoint
	}
	host, port := u.Hostname(), u.Port()
	if port != "" && port != "443" {
		return nil, ErrEndpoint
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil, ErrEndpoint
	}
	if !knownHost(host) {
		return nil, ErrEndpoint
	}
	return u, nil
}

func knownHost(h string) bool {
	if slices.Contains(pushHosts, h) {
		return true
	}
	for _, s := range pushSuffixes {
		if labels, ok := strings.CutSuffix(h, s); ok && validLabels(labels) {
			return true
		}
	}
	return false
}

// validLabels: one or more DNS labels, lowercase letters, digits and inner
// hyphens only.
func validLabels(s string) bool {
	if s == "" {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range []byte(l) {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// decodeKey reads base64url with or without padding.
func decodeKey(s string) ([]byte, error) {
	return b64.DecodeString(strings.TrimRight(s, "="))
}

// ParseSubscription checks a subscription the page posted.
func ParseSubscription(endpoint, p256dh, auth string, test TestHosts) (Subscription, error) {
	if _, err := CheckEndpoint(endpoint, test); err != nil {
		return Subscription{}, err
	}
	p, err1 := decodeKey(p256dh)
	a, err2 := decodeKey(auth)
	if err1 != nil || err2 != nil || len(p) != pointLen || len(a) != authLen {
		return Subscription{}, ErrKeys
	}
	if _, err := ecdh.P256().NewPublicKey(p); err != nil {
		return Subscription{}, ErrKeys
	}
	return Subscription{Endpoint: endpoint, P256DH: b64.EncodeToString(p), Auth: b64.EncodeToString(a)}, nil
}

func (s Subscription) keys() (p256dh, auth []byte, err error) {
	p, err1 := decodeKey(s.P256DH)
	a, err2 := decodeKey(s.Auth)
	if err1 != nil || err2 != nil {
		return nil, nil, ErrKeys
	}
	return p, a, nil
}
