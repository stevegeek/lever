package remoteproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/lever/internal/webpush"
)

// fakeSender records pushes instead of sending them.
type fakeSender struct {
	mu   sync.Mutex
	sent []string // "<endpoint> <payload>"
	err  func(webpush.Subscription) error
	// hook, when set, runs before the send is recorded, with its context.
	hook func(ctx context.Context)
}

func (f *fakeSender) Send(ctx context.Context, s webpush.Subscription, p []byte) error {
	if f.hook != nil {
		f.hook(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, s.Endpoint+" "+string(p))
	if f.err != nil {
		return f.err(s)
	}
	return nil
}

func (f *fakeSender) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// pushConfig is chatConfig (operator op@x, contact c@x with agents [w1], see [w2]) with push on.
func pushConfig(t *testing.T, hub *pageHub, fs *fakeSender, lines *lockedLines) (Config, *Push) {
	t.Helper()
	cfg := chatConfig(t, hub)
	if lines != nil {
		cfg.Audit = lines.add
	}
	p, err := NewPush(PushOptions{Dir: filepath.Join(t.TempDir(), "push"), Subject: "mailto:op@example.com",
		Logins: cfg.AllowedUsers, Audit: cfg.Audit, Sender: fs})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Push = p
	return cfg, p
}

func subBody(id string) string {
	return `{"endpoint":"https://fcm.googleapis.com/fcm/send/` + id + `","expirationTime":null,"keys":{"p256dh":"` + testP256 + `","auth":"` + testAuth + `"}}`
}

func pushWrite(h http.Handler, login, method, body string, hdr ...string) *httptest.ResponseRecorder {
	req := proxyRequest(method, pushSubsPath, strings.NewReader(body))
	req.Header.Set("Tailscale-User-Login", login)
	for k, v := range map[string]string{"Origin": "https://" + testServeHost, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"} {
		req.Header.Set(k, v)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			req.Header.Del(hdr[i])
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

func TestPushKeyRoute(t *testing.T) {
	hub := newPageHub(t)
	cfg, p := pushConfig(t, hub, &fakeSender{}, nil)
	h := NewHandler(cfg)
	for _, login := range []string{chatOp, "c@x"} {
		rw := chatDo(h, login, "GET", pushKeyPath)
		var body map[string]string
		if rw.Code != 200 || json.Unmarshal(rw.Body.Bytes(), &body) != nil || body["key"] != p.PublicKey() || len(body["key"]) != 87 {
			t.Fatalf("%s: %d %s", login, rw.Code, rw.Body.String())
		}
		for k, v := range map[string]string{"Cache-Control": "no-store", "Content-Security-Policy": "sandbox", "Content-Type": "application/json"} {
			if rw.Header().Get(k) != v {
				t.Errorf("%s = %q", k, rw.Header().Get(k))
			}
		}
	}
	if rw := chatDo(h, "", "GET", pushKeyPath); rw.Code == 200 {
		t.Fatal("an unverified request got the key route")
	}
}

func TestPushOffHasNoRoutesAndNoWorker(t *testing.T) {
	hub := newPageHub(t)
	h := NewHandler(chatConfig(t, hub))
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", pushKeyPath, 404}, {"GET", chatSWPath, 404}, {"POST", pushSubsPath, 405}, {"DELETE", pushSubsPath, 405}} {
		rw := chatDo(h, chatOp, tc.method, tc.path, "Origin", "https://"+testServeHost)
		if rw.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rw.Code, tc.want)
		}
	}
	if len(hub.reached()) != 0 {
		t.Fatalf("the hub was asked: %v", hub.reached())
	}
}

func TestPushSubscribeAndUnsubscribe(t *testing.T) {
	hub := newPageHub(t)
	var lines lockedLines
	cfg, p := pushConfig(t, hub, &fakeSender{}, &lines)
	h := NewHandler(cfg)
	if rw := pushWrite(h, chatOp, "POST", subBody("op1")); rw.Code != 201 {
		t.Fatalf("subscribe %d %s", rw.Code, rw.Body.String())
	}
	if rw := pushWrite(h, "c@x", "POST", subBody("c1")); rw.Code != 201 {
		t.Fatalf("contact subscribe %d", rw.Code)
	}
	if len(p.store.Subs(chatOp)) != 1 || len(p.store.Subs("c@x")) != 1 {
		t.Fatal("not stored per login")
	}
	if rw := pushWrite(h, chatOp, "DELETE", `{"endpoint":"https://fcm.googleapis.com/fcm/send/c1"}`); rw.Code != 200 {
		t.Fatalf("delete %d", rw.Code)
	}
	if len(p.store.Subs("c@x")) != 1 {
		t.Fatal("the operator deleted the contact's subscription")
	}
	if rw := pushWrite(h, chatOp, "DELETE", `{"endpoint":"https://fcm.googleapis.com/fcm/send/op1"}`); rw.Code != 200 || len(p.store.Subs(chatOp)) != 0 {
		t.Fatalf("own delete %d", rw.Code)
	}
	b, _ := json.Marshal(lines.all())
	if strings.Contains(string(b), "/fcm/send/") || strings.Contains(string(b), testAuth) {
		t.Fatalf("the audit carries an endpoint path or a key: %s", b)
	}
	if !strings.Contains(string(b), string(DecisionPushSubscribe)) || !strings.Contains(string(b), string(DecisionPushUnsubscribe)) {
		t.Fatalf("audit %s", b)
	}
}

func TestPushRoutesEndpointMovesBetweenLogins(t *testing.T) {
	hub := newPageHub(t)
	cfg, p := pushConfig(t, hub, &fakeSender{}, nil)
	h := NewHandler(cfg)
	pushWrite(h, chatOp, "POST", subBody("same"))
	pushWrite(h, "c@x", "POST", subBody("same"))
	if len(p.store.Subs(chatOp)) != 0 || len(p.store.Subs("c@x")) != 1 {
		t.Fatal("one device endpoint must belong to the last login only")
	}
}

func TestPushSubscribeRefusals(t *testing.T) {
	hub := newPageHub(t)
	cfg, p := pushConfig(t, hub, &fakeSender{}, nil)
	h := NewHandler(cfg)
	big := `{"endpoint":"https://fcm.googleapis.com/fcm/send/` + strings.Repeat("a", 5000) + `"}`
	for name, tc := range map[string]struct {
		method, body string
		hdr          []string
		code         int
		word         string
	}{
		"put":            {"PUT", subBody("x"), nil, 405, "method"},
		"no origin":      {"POST", subBody("x"), []string{"Origin", ""}, 403, "origin"},
		"null origin":    {"POST", subBody("x"), []string{"Origin", "null"}, 403, ""},
		"same-site":      {"POST", subBody("x"), []string{"Sec-Fetch-Site", "same-site"}, 403, ""},
		"form type":      {"POST", subBody("x"), []string{"Content-Type", "application/x-www-form-urlencoded"}, 415, "content-type"},
		"text type":      {"POST", subBody("x"), []string{"Content-Type", "text/plain"}, 415, "content-type"},
		"too large":      {"POST", big, nil, 413, "too-large"},
		"not json":       {"POST", `{`, nil, 400, "bad-json"},
		"http endpoint":  {"POST", strings.Replace(subBody("x"), "https://", "http://", 1), nil, 400, "endpoint"},
		"internal host":  {"POST", strings.Replace(subBody("x"), "fcm.googleapis.com", "10.0.0.1", 1), nil, 400, "endpoint"},
		"loopback":       {"POST", strings.Replace(subBody("x"), "https://fcm.googleapis.com", "https://127.0.0.1:9447", 1), nil, 400, "endpoint"},
		"bad keys":       {"POST", strings.Replace(subBody("x"), testAuth, "AAAA", 1), nil, 400, "keys"},
		"unknown field":  {"POST", strings.Replace(subBody("x"), `"keys"`, `"extra":1,"keys"`, 1), nil, 400, "bad-json"},
		"endpoint twice": {"POST", strings.Replace(subBody("x"), `"keys"`, `"endpoint":"https://fcm.googleapis.com/fcm/send/y","keys"`, 1), nil, 400, "bad-json"},
		"auth twice":     {"POST", strings.Replace(subBody("x"), `"auth"`, `"auth":"`+testAuth+`","auth"`, 1), nil, 400, "bad-json"},
		"trailing data":  {"POST", subBody("x") + `{}`, nil, 400, "bad-json"},
		"port 443":       {"POST", strings.Replace(subBody("x"), "fcm.googleapis.com", "fcm.googleapis.com:443", 1), nil, 400, "endpoint"},
	} {
		rw := pushWrite(h, chatOp, tc.method, tc.body, tc.hdr...)
		if rw.Code != tc.code || tc.word != "" && !strings.Contains(rw.Body.String(), `"error":"`+tc.word+`"`) {
			t.Errorf("%s: %d %s", name, rw.Code, rw.Body.String())
		}
	}
	if len(p.store.Logins()) != 0 {
		t.Fatal("a refused request stored a subscription")
	}
}

func TestServiceWorkerServedOnlyWithPush(t *testing.T) {
	hub := newPageHub(t)
	cfg, _ := pushConfig(t, hub, &fakeSender{}, nil)
	h := NewHandler(cfg)
	for _, login := range []string{chatOp, "c@x"} {
		rw := chatDo(h, login, "GET", chatSWPath)
		if rw.Code != 200 || rw.Header().Get("Content-Type") != "text/javascript; charset=utf-8" ||
			rw.Header().Get("Content-Security-Policy") != chatCSPFor(testServeHost) || rw.Header().Get("Service-Worker-Allowed") != "" {
			t.Fatalf("%s: %d %v", login, rw.Code, rw.Header())
		}
	}
}

func TestChatCSPAllowsTheWorkerFromLeverOnly(t *testing.T) {
	if csp := chatCSPFor("mac.ts.net"); !strings.Contains(csp, "worker-src mac.ts.net/lever/;") {
		t.Fatalf("CSP %s", csp)
	}
	if csp := chatCSPFor("[::1]:8445"); !strings.Contains(csp, "worker-src 'self';") {
		t.Fatalf("fallback CSP %s", csp)
	}
}

func TestPushKeyRouteIsReadOnly(t *testing.T) {
	hub := newPageHub(t)
	cfg, _ := pushConfig(t, hub, &fakeSender{}, nil)
	h := NewHandler(cfg)
	for _, m := range []string{"POST", "DELETE", "PUT"} {
		rw := chatDo(h, chatOp, m, pushKeyPath, "Origin", "https://"+testServeHost)
		if rw.Code != http.StatusMethodNotAllowed || rw.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d %v", m, rw.Code, rw.Header())
		}
	}
}

func TestPushStrictBodyKeepsNesting(t *testing.T) {
	var in pushSubBody
	if err := decodePushSubBody([]byte(subBody("x")), &in); err != nil || in.Keys.Auth != testAuth {
		t.Fatalf("%v %+v", err, in)
	}
	// The same key in two different objects is not a duplicate.
	b := `{"endpoint":"https://fcm.googleapis.com/fcm/send/x","keys":{"endpoint":"x","p256dh":"` + testP256 + `","auth":"` + testAuth + `"}}`
	if err := noDuplicateKeys([]byte(b)); err != nil {
		t.Fatal(err)
	}
}
