package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublicAddr(t *testing.T) {
	for _, s := range []string{"142.250.180.10", "17.57.144.10", "2a00:1450:4009:81f::200a"} {
		if !publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range []string{"10.0.0.5", "172.16.1.1", "192.168.1.1", "127.0.0.1", "0.0.0.0", "169.254.169.254",
		"100.100.100.100", "100.64.0.1", "192.0.2.1", "198.18.0.1", "224.0.0.1", "255.255.255.255", "240.0.0.1",
		"::1", "::", "fe80::1", "fd7a:115c:a1e0::1", "fc00::1", "ff02::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
		"64:ff9b::a00:1", "64:ff9b:1::1", "2002:a00:1::", "2001::1", "2001:db8::1"} {
		if publicAddr(netip.MustParseAddr(s)) {
			t.Errorf("%s admitted", s)
		}
	}
}

func TestControlRefusesNonPublic(t *testing.T) {
	c := control(TestHosts{"127.0.0.1:9447": true})
	for addr, ok := range map[string]bool{
		"142.250.180.10:443": true, "142.250.180.10:80": false, "10.0.0.1:443": false,
		"[::ffff:10.0.0.1]:443": false, "127.0.0.1:443": false, "127.0.0.1:9447": true, "bogus": false,
	} {
		if err := c("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("control(%s) = %v, want ok=%v", addr, err, ok)
		}
	}
}

// TestDialerRefusesNonPublicAnswers: what the resolver answers decides
// nothing by itself; only public addresses are dialled, on 443.
func TestDialerRefusesNonPublicAnswers(t *testing.T) {
	var mu sync.Mutex
	var dialled []string
	d := &dialer{
		dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			mu.Lock()
			dialled = append(dialled, addr)
			mu.Unlock()
			return nil, errors.New("test: no network")
		},
	}
	for name, tc := range map[string]struct {
		answer []string
		addr   string
		want   []string
	}{
		"private only":   {[]string{"10.0.0.5"}, "fcm.googleapis.com:443", nil},
		"tailnet":        {[]string{"100.101.102.103"}, "fcm.googleapis.com:443", nil},
		"mapped private": {[]string{"::ffff:192.168.0.1"}, "fcm.googleapis.com:443", nil},
		"nat64":          {[]string{"64:ff9b::7f00:1"}, "fcm.googleapis.com:443", nil},
		"mixed":          {[]string{"10.0.0.5", "142.250.180.10"}, "fcm.googleapis.com:443", []string{"142.250.180.10:443"}},
		"other port":     {[]string{"142.250.180.10"}, "fcm.googleapis.com:80", nil},
	} {
		dialled = nil
		d.lookup = func(context.Context, string) ([]netip.Addr, error) {
			var out []netip.Addr
			for _, a := range tc.answer {
				out = append(out, netip.MustParseAddr(a))
			}
			return out, nil
		}
		_, err := d.DialContext(context.Background(), "tcp", tc.addr)
		if err == nil {
			t.Errorf("%s: dial succeeded", name)
		}
		if tc.want == nil && !errors.Is(err, ErrAddress) {
			t.Errorf("%s: err %v, want ErrAddress", name, err)
		}
		if strings.Join(dialled, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: dialled %v, want %v", name, dialled, tc.want)
		}
	}
}

// receiver is a push service on loopback, admitted through TestHosts.
type receiver struct {
	*httptest.Server
	ua     *ecdh.PrivateKey
	auth   []byte
	status int
	mu     sync.Mutex
	got    []*http.Request
	bodies [][]byte
}

func newReceiver(t *testing.T, status int) (*receiver, Subscription, TestHosts) {
	r := &receiver{status: status}
	r.ua, _ = ecdh.P256().GenerateKey(rand.Reader)
	r.auth = make([]byte, 16)
	rand.Read(r.auth)
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got, r.bodies = append(r.got, req), append(r.bodies, b)
		r.mu.Unlock()
		if r.status == http.StatusFound {
			w.Header().Set("Location", "http://10.0.0.1/steal")
		}
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.Close)
	host := strings.TrimPrefix(r.URL, "http://")
	th, err := ParseTestHosts(host)
	if err != nil {
		t.Fatal(err)
	}
	return r, Subscription{Endpoint: r.URL + "/push/op", P256DH: b64.EncodeToString(r.ua.PublicKey().Bytes()), Auth: b64.EncodeToString(r.auth)}, th
}

func TestSendDeliversAnEncryptedSignedPush(t *testing.T) {
	r, sub, th := newReceiver(t, http.StatusCreated)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	s := &Sender{Key: key, Subject: "mailto:op@example.com", Test: th, Client: NewClient(th), Now: func() time.Time { return now }}
	if err := s.Send(context.Background(), sub, []byte(`{"v":1,"agent":"w1"}`)); err != nil {
		t.Fatal(err)
	}
	req, body := r.got[0], r.bodies[0]
	for k, v := range map[string]string{"Content-Encoding": "aes128gcm", "Ttl": "3600", "Urgency": "normal", "Content-Type": "application/octet-stream"} {
		if req.Header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, req.Header.Get(k), v)
		}
	}
	if err := VerifyVAPID(req.Header.Get("Authorization"), r.URL, now); err != nil {
		t.Errorf("VAPID: %v (aud must be the endpoint origin)", err)
	}
	if len(body) > 4096 {
		t.Errorf("body %d bytes", len(body))
	}
	pt, err := Decrypt(r.ua, r.auth, body)
	if err != nil || string(pt) != `{"v":1,"agent":"w1"}` {
		t.Fatalf("decrypt %q %v", pt, err)
	}
}

func TestSendStatuses(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for status, want := range map[int]struct{ gone, ok bool }{
		201: {false, true}, 200: {false, true}, 404: {true, false}, 410: {true, false}, 429: {false, false}, 500: {false, false}, 302: {false, false},
	} {
		r, sub, th := newReceiver(t, status)
		s := &Sender{Key: key, Subject: "mailto:x@y", Test: th, Client: NewClient(th)}
		err := s.Send(context.Background(), sub, []byte(`{}`))
		if (err == nil) != want.ok || Gone(err) != want.gone {
			t.Errorf("HTTP %d: err %v, gone %v", status, err, Gone(err))
		}
		if !want.ok && StatusOf(err) != status {
			t.Errorf("HTTP %d: StatusOf = %d", status, StatusOf(err))
		}
		if len(r.got) != 1 {
			t.Errorf("HTTP %d: %d requests (a redirect is never followed)", status, len(r.got))
		}
	}
}

func TestSendRefusesAnEndpointOutsideTheList(t *testing.T) {
	_, sub, _ := newReceiver(t, 201)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	s := &Sender{Key: key, Subject: "mailto:x@y"} // no test hosts: production
	if err := s.Send(context.Background(), sub, []byte(`{}`)); !errors.Is(err, ErrEndpoint) {
		t.Fatalf("err %v", err)
	}
}

func TestSendErrorNamesTheHostOnly(t *testing.T) {
	_, sub, th := newReceiver(t, 201)
	sub.Endpoint = strings.Replace(sub.Endpoint, "/push/op", "/push/SECRET-TOKEN", 1)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Sender{Key: key, Subject: "mailto:x@y", Test: th, Client: NewClient(th)}).Send(ctx, sub, []byte(`{}`))
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("err %v must not carry the endpoint path", err)
	}
}
